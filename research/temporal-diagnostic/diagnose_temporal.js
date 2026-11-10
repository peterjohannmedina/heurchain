// Offline, deterministic temporal-miss diagnostic over frozen DEV artifacts.
// Reads only: DEV labels/questions, frozen answers, frozen retrieval receipts, frozen verdicts, DEV source ledger.
// No model calls, no holdout reads, no writes outside this folder.
"use strict";
const fs = require("fs");
const path = require("path");

const SCORING = process.env.SCORING_ROOT;
const LUNA = process.env.LUNA_ROOT;
const OUT = process.env.RESEARCH_OUTPUT_ROOT;
if (!SCORING || !LUNA || !OUT) throw new Error("Set SCORING_ROOT, LUNA_ROOT and RESEARCH_OUTPUT_ROOT to separately authorized offline artifact directories");

const readJson = (p) => JSON.parse(fs.readFileSync(p, "utf8"));
const readJsonl = (p) => fs.readFileSync(p, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l));

// ---- inputs
const labels = new Map(readJson(`${SCORING}/inputs/FULL-DEV-LABELS.json`).map((r) => [r.id, r]));
const questions = new Map(readJson(`${SCORING}/inputs/FULL-DEV-QUESTIONS.json`).map((r) => [r.id, r.question]));
const hAnswers = new Map(readJsonl(`${SCORING}/inputs/BASELINE-DEV-ANSWERS-COMPLETE701.jsonl`).map((r) => [r.question.id, r]));
const mAnswers = new Map(readJsonl(`${SCORING}/mem0-oss-v3-comparison-v1/inputs/MEM0-DEV-ANSWERS-COMPLETE701.jsonl`).map((r) => [r.question.id, r]));
const mVerdicts = new Map(readJsonl(`${SCORING}/mem0-oss-v3-comparison-v1/judge-resume-355-v1/FINAL543-VERDICTS.jsonl`).map((r) => [r.id, r.binary_score]));
const hVerdicts = new Map();
const qdir = `${SCORING}/mem0-binary-judge-v3/run-001/questions`;
for (const d of fs.readdirSync(qdir)) {
  const v = readJson(`${qdir}/${d}/VERDICT.json`);
  hVerdicts.set(v.id, v.binary_score);
}
const sources = new Map();
const byDia = new Map();
for (const r of readJsonl(`${LUNA}/inputs/FULL-DEV-SOURCE.jsonl`)) {
  const s = r.store, m = s.metadata;
  const rec = { id: s.id, sample: m.sample_id, dia: m.dia_id, session: m.session_number, ts: m.session_timestamp_literal, text: s.text };
  sources.set(s.id, rec);
  byDia.set(`${m.sample_id}|${m.dia_id}`, rec);
}
const retrievals = new Map();
const rdir = `${LUNA}/native-evidence/retrievals`;
for (const f of fs.readdirSync(rdir)) {
  const j = readJson(`${rdir}/${f}`);
  retrievals.set(j.question.id, j.retrieval);
}

// ---- integrity checks
const checks = {
  hVerdicts: hVerdicts.size, mVerdicts: mVerdicts.size, hAnswers: hAnswers.size, mAnswers: mAnswers.size,
  retrievals: retrievals.size, sources: sources.size, packedNotInRetrieval: 0, packedOrderMismatch: 0,
};
for (const [id, a] of hAnswers) {
  const ret = retrievals.get(id);
  const ids = ret.results.map((x) => x.id);
  const sel = a.packing.selected_source_ids;
  if (sel.some((s) => !ids.includes(s))) checks.packedNotInRetrieval++;
  const expected = ids.filter((x) => !a.packing.excluded_source_ids.includes(x));
  if (JSON.stringify(expected) !== JSON.stringify(sel)) checks.packedOrderMismatch++;
}

// ---- date helpers
const MONTHS = ["january","february","march","april","may","june","july","august","september","october","november","december"];
const MON = "(january|february|march|april|may|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sept|sep|oct|nov|dec)";
const mIdx = (s) => { s = s.toLowerCase(); return MONTHS.findIndex((m) => m.startsWith(s.slice(0, 3))) + 1; };
function parseDates(text) {
  const t = String(text || "");
  const out = [];
  const seen = new Set();
  const push = (y, m, d) => { const k = `${y}-${m}-${d}`; if (!seen.has(k)) { seen.add(k); out.push({ y, m, d }); } };
  let r;
  const dmy = new RegExp(`\\b(\\d{1,2})(?:st|nd|rd|th)?\\s+(?:of\\s+)?${MON}\\.?,?\\s+(\\d{4})`, "gi");
  while ((r = dmy.exec(t))) push(+r[3], mIdx(r[2]), +r[1]);
  const mdy = new RegExp(`\\b${MON}\\.?\\s+(\\d{1,2})(?:st|nd|rd|th)?,?\\s+(\\d{4})`, "gi");
  while ((r = mdy.exec(t))) push(+r[3], mIdx(r[1]), +r[2]);
  const my = new RegExp(`\\b${MON}\\.?,?\\s+(\\d{4})`, "gi");
  while ((r = my.exec(t))) if (![...out].some((o) => o.y === +r[2] && o.m === mIdx(r[1]))) push(+r[2], mIdx(r[1]), null);
  const y = /\b(19\d{2}|20\d{2})\b/g;
  while ((r = y.exec(t))) if (!out.some((o) => o.y === +r[1])) push(+r[1], null, null);
  return out;
}
const sessionDate = (ts) => { const d = parseDates(ts)[0]; return d && d.d ? d : null; };
const sameDay = (a, b) => a && b && a.y === b.y && a.m === b.m && a.d === b.d && a.d != null;
const RELATIVE = /\b(yesterday|today|tonight|tomorrow|last\s+(?:week|weekend|month|year|night|summer|winter|spring|fall|autumn|monday|tuesday|wednesday|thursday|friday|saturday|sunday)|next\s+(?:week|weekend|month|year)|this\s+(?:week|weekend|month|year|morning|evening)|(?:a|an|one|two|three|four|five|few|couple(?:\s+of)?)\s+(?:days?|weeks?|months?|years?)\s+ago|the\s+other\s+day|recently|earlier\s+this\s+(?:week|month|year))\b/i;
const RELATIVE_GOLD = /\b(before|after|week of|weekend of|the\s+(?:week|weekend|month|sunday|saturday|friday|monday|tuesday|wednesday|thursday)\s+(?:before|after|of))\b/i;
const hasAbsolute = (s) => parseDates(s).length > 0;
const isUnknown = (s) => /^\s*unknown\b/i.test(String(s || "").trim()) || /^\s*\{?\s*"?answer"?\s*:\s*"?unknown/i.test(String(s || ""));

// ---- per-question rows
function evidenceInfo(id, sample) {
  const lab = labels.get(id);
  const ret = retrievals.get(id);
  const a = hAnswers.get(id);
  const sel = new Set(a.packing.selected_source_ids);
  const rankOf = new Map(ret.results.map((x, i) => [x.id, i + 1]));
  const ev = (lab.valid_evidence || []).map((dia) => {
    const src = byDia.get(`${sample}|${dia}`);
    const sid = src ? src.id : null;
    return {
      dia, ts: src ? src.ts : null, sessionDate: src ? sessionDate(src.ts) : null,
      rank: sid && rankOf.has(sid) ? rankOf.get(sid) : null,
      packed: sid ? sel.has(sid) : false,
      text: src ? src.text.replace(/^\[[^\]]*\]\n/, "").slice(0, 260) : null,
    };
  });
  const packedN = ev.filter((e) => e.packed).length;
  const coverage = ev.length === 0 ? "no-labels" : packedN === ev.length ? "all" : packedN > 0 ? "partial" : "none";
  const retrievedNotPacked = ev.some((e) => e.rank != null && !e.packed);
  return { ev, coverage, retrievedNotPacked, packedUnits: sel.size, excludedUnits: a.packing.excluded_source_ids.length };
}

function classify(row) {
  const ans = row.h_answer, gold = row.gold;
  if (row.coverage === "none") return row.retrievedNotPacked ? "evidence retrieved but cut by the byte cap" : "evidence not retrieved";
  if (isUnknown(ans)) return row.coverage === "partial" ? "abstained, partial evidence" : "abstained with evidence in context";
  const aDates = parseDates(ans), gDates = parseDates(gold);
  const sess = row.evidence.map((e) => e.sessionDate).filter(Boolean);
  if (RELATIVE.test(ans) && !hasAbsolute(ans)) return "relative time left unresolved";
  const usedSession = aDates.some((ad) => sess.some((sd) => sameDay(ad, sd)));
  const goldIsSession = gDates.some((gd) => sess.some((sd) => sameDay(gd, sd)));
  if (usedSession && !RELATIVE_GOLD.test(gold) && !(goldIsSession && aDates.some((ad) => gDates.some((gd) => sameDay(ad, gd))))) return "session date given instead of event date";
  if (usedSession && RELATIVE_GOLD.test(gold)) return "session date given, gold is offset from it";
  if (row.coverage === "partial") return "wrong answer, partial evidence";
  return "wrong date or period with evidence in context";
}

const rows = [];
for (const [id, lab] of labels) {
  if (!hVerdicts.has(id)) continue; // non-scored (adversarial) questions
  const sample = lab.sample_id;
  const info = evidenceInfo(id, sample);
  rows.push({
    id, sample, category: lab.category, question: questions.get(id), gold: lab.answer,
    h_answer: hAnswers.get(id).answer, m_answer: mAnswers.get(id).answer,
    h: hVerdicts.get(id), m: mVerdicts.get(id),
    coverage: info.coverage, retrievedNotPacked: info.retrievedNotPacked,
    packedUnits: info.packedUnits, excludedUnits: info.excludedUnits, evidence: info.ev,
  });
}

// ---- aggregates
const CATS = { 1: "multi-hop", 2: "temporal", 3: "open-domain", 4: "single-hop" };
const paired = {};
const coverageByCat = {};
for (const r of rows) {
  const c = CATS[r.category];
  paired[c] ??= { n: 0, both: 0, h_only: 0, m_only: 0, neither: 0 };
  const p = paired[c]; p.n++;
  if (r.h && r.m) p.both++; else if (r.h) p.h_only++; else if (r.m) p.m_only++; else p.neither++;
  coverageByCat[c] ??= { n: 0, all: 0, partial: 0, none: 0, "no-labels": 0, cutByCap: 0, h_correct_by_coverage: {} };
  const cv = coverageByCat[c]; cv.n++; cv[r.coverage]++;
  if (r.coverage !== "all" && r.retrievedNotPacked) cv.cutByCap++;
  cv.h_correct_by_coverage[r.coverage] ??= [0, 0];
  cv.h_correct_by_coverage[r.coverage][0] += r.h; cv.h_correct_by_coverage[r.coverage][1]++;
}
const temporal = rows.filter((r) => r.category === 2);
const byConv = {};
for (const r of temporal) { byConv[r.sample] ??= { n: 0, h: 0, m: 0 }; byConv[r.sample].n++; byConv[r.sample].h += r.h; byConv[r.sample].m += r.m; }
const hMiss = temporal.filter((r) => !r.h).map((r) => ({ ...r, cls: classify(r) }));
const classCounts = {};
for (const r of hMiss) {
  classCounts[r.cls] ??= { total: 0, mem0_correct: 0 };
  classCounts[r.cls].total++; classCounts[r.cls].mem0_correct += r.m;
}
// answer-style signals across all temporal questions, both arms
const style = (key) => {
  const s = { n: temporal.length, unknown: 0, relative_only: 0, has_absolute: 0 };
  for (const r of temporal) {
    const a = r[key];
    if (isUnknown(a)) s.unknown++; else if (RELATIVE.test(a) && !hasAbsolute(a)) s.relative_only++; else if (hasAbsolute(a)) s.has_absolute++;
  }
  return s;
};

// ---- relative-phrase split and explicit-date share of the evidence each arm handed the answerer
const MEM0_REQ = process.env.MEM0_REQUESTS_ROOT;
if (!MEM0_REQ) throw new Error("Set MEM0_REQUESTS_ROOT to the separately authorized offline Mem0 request directory");
const RELATIVE_EVIDENCE = new RegExp(`(?:${RELATIVE.source})|\\bthis\\s+past\\s+(?:week|weekend)\\b|\\b\\w+\\s+(?:days?|weeks?|months?|years?)\\s+ago\\b`, "i");
const mem0Units = new Map();
for (const d of fs.readdirSync(MEM0_REQ)) {
  const p = `${MEM0_REQ}/${d}/REQUEST.json`;
  if (!fs.existsSync(p)) continue;
  const j = readJson(p);
  const prompt = j.packing.prompt;
  const qid = (JSON.stringify(j.question || "").match(/conv-\d+-qa-\d+/) || JSON.stringify(j).match(/conv-\d+-qa-\d+/))[0];
  mem0Units.set(qid, JSON.parse(prompt.slice(prompt.indexOf("\n[") + 1, prompt.lastIndexOf("]") + 1)).map((u) => u.text));
}
const split = { relative: { n: 0, h: 0, m: 0 }, other: { n: 0, h: 0, m: 0 } };
const dateShare = { heurchain: [0, 0], mem0: [0, 0] };
for (const r of temporal) {
  const rel = r.evidence.some((e) => e.text && RELATIVE_EVIDENCE.test(e.text));
  const s = split[rel ? "relative" : "other"]; s.n++; s.h += r.h; s.m += r.m;
  for (const sid of hAnswers.get(r.id).packing.selected_source_ids) {
    const body = sources.get(sid).text.replace(/^\[[^\]]*\]\n/, "");
    dateShare.heurchain[0] += hasAbsolute(body) ? 1 : 0; dateShare.heurchain[1]++;
  }
  for (const t of mem0Units.get(r.id) || []) { dateShare.mem0[0] += hasAbsolute(t) ? 1 : 0; dateShare.mem0[1]++; }
}

const result = {
  generated_utc: new Date().toISOString(),
  temporal_split_by_relative_evidence: split,
  temporal_units_stating_explicit_date: {
    heurchain_turn_text_excluding_session_header: dateShare.heurchain,
    mem0_memory_text: dateShare.mem0,
    mem0_requests_loaded: mem0Units.size,
  },
  scope: "DEV only (conv-26, conv-30, conv-41, conv-50); frozen artifacts; no model calls; no holdout reads",
  integrity: checks,
  paired_by_category: paired,
  evidence_coverage_by_category: coverageByCat,
  temporal_by_conversation: byConv,
  temporal_answer_style: { heurchain: style("h_answer"), mem0: style("m_answer") },
  heurchain_temporal_misses: { total: hMiss.length, classes: classCounts },
  heurchain_temporal_miss_rows: hMiss.map((r) => ({
    id: r.id, cls: r.cls, mem0_correct: r.m, question: r.question, gold: r.gold,
    heurchain_answer: r.h_answer, mem0_answer: r.m_answer, coverage: r.coverage,
    evidence: r.evidence.map((e) => ({ dia: e.dia, session_ts: e.ts, rank: e.rank, packed: e.packed, text: e.text })),
  })),
};
fs.writeFileSync(path.join(OUT, "TEMPORAL-MISS-DIAGNOSTIC-2026-10-07.json"), JSON.stringify(result, null, 2));
console.log(JSON.stringify({ integrity: checks, split, dateShare, mem0Requests: mem0Units.size }, null, 1));
for (const [c, v] of Object.entries(coverageByCat)) console.log(c, JSON.stringify(v));
