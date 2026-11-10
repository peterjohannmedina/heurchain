All three systems have701 saved DEV answers and543 uniquely paired binary verdicts. Source corpus:2019 turns/117 batches; four DEV conversations; six heldout conversations remain sealed.

| System | Correct /543 | Local judge score |
|---|---:|---:|
| Frozen HeurChain | 360/543 | 66.30% |
| Revised HeurChain | 363/543 | 66.85% |
| Mem0 OSS | 369/543 | 67.96% |

| Category | Questions | Frozen | Revised | Mem0 | Revised − frozen | Revised − Mem0 |
|---|---:|---:|---:|---:|---:|---:|
| Multi-hop | 106 | 50 | 57 | 57 | +7 | +0 |
| Temporal | 122 | 76 | 72 | 94 | -4 | -22 |
| Open-domain | 28 | 9 | 8 | 6 | -1 | +2 |
| Single-hop | 287 | 225 | 226 | 212 | +1 | +14 |

Paired differences (same question IDs):

| Right vs left | Both correct | Both wrong | Left only correct | Right only correct | Net right | Difference, pp |
|---|---:|---:|---:|---:|---:|---:|
| candidate vs baseline | 340 | 160 | 20 | 23 | +3 | +0.55 |
| Mem0 vs baseline | 293 | 107 | 67 | 76 | +9 | +1.66 |
| candidate vs Mem0 | 294 | 105 | 75 | 69 | -6 | -1.10 |

Successful-call usage estimates, in credits under the frozen rate schedule:

| System | Answers | Binary judge | Native extraction | Known successful total |
|---|---:|---:|---:|---:|
| baseline | 14.3126140 | 2.6570900 | 0.0000000 | 16.9697040 |
| candidate | 13.8329955 | 2.6074965 | 0.0000000 | 16.4404920 |
| Mem0 | 20.3208755 | 2.7623480 | 4.6726850 | 27.7559085 |

These are estimates from successful reported usage, excluding unknown failed charges. They are not verified invoices or provider billing caps. Full token counts, failed attempt receipts and category/conversation pair tables are in THREE-WAY-COMPARISON.json.

Candidate terminal: 2026-10-07T05:12:31.064348+00:00. Candidate cumulative attempts:702 answers+543 judge=1245. Continuation reused native capture and made zero new native/embedding calls. All successful answer/judge owned workers are reaped; fresh candidate cleanup at2026-10-07T05:19:26.671145+00:00 found no owned listeners or controllers.

This is one local gpt-6-luna/low/default judge pass using the historical full-reference rubric. Human/source-level factual accuracy and a generalizable advantage are unestablished. No heldout data was opened, no failed/unknown attempt was scored wrong, and no new inference was performed during this audit. Mem0 OSS2026 configuration does not replicate the published2025 system.

Frozen baseline commit: 216e39eb8d91b69a75452037efd96bd597b5ffb6. Reviewed candidate commit: 7636913af7c15406ffd03db97ffac34a9d008b36. Candidate continuation manifest: c00a9ded5c30bd2331d191b6e74f63d05b2081cc805c9c827ae971bb566cb2f7. Mem0 OSS commit: 94c3fe9f238f3dbf29c9ce98643bd71eb13077cd.

Future supervisor packet: [machine-local path omitted]
