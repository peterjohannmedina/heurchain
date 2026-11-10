All counts below are correct answers under the same saved single-pass local binary judge. Primary population543; the158 adversarial questions are excluded. Every arm completed701 DEV answers.

| Category | n | Frozen | Revised | Mem0 | Revised − frozen | Revised − Mem0 |
|---|---:|---:|---:|---:|---:|---:|
| Multi-hop | 106 | 50 | 57 | 57 | +7 | +0 |
| Temporal | 122 | 76 | 72 | 94 | -4 | -22 |
| Open-domain | 28 | 9 | 8 | 6 | -1 | +2 |
| Single-hop | 287 | 225 | 226 | 212 | +1 | +14 |

Conversation-level paired changes. Wins/losses mean revised-only correct/opponent-only correct on the identical question ID.

| Conversation | n | Frozen | Revised | Mem0 | Revised vs frozen: wins/losses (net) | Revised vs Mem0: wins/losses (net) |
|---|---:|---:|---:|---:|---:|---:|
| conv-26 | 152 | 101 | 96 | 100 | 4/9 (-5) | 22/26 (-4) |
| conv-30 | 81 | 56 | 56 | 52 | 4/4 (+0) | 13/9 (+4) |
| conv-41 | 152 | 97 | 108 | 110 | 13/2 (+11) | 16/18 (-2) |
| conv-50 | 158 | 106 | 103 | 107 | 2/5 (-3) | 18/22 (-4) |

The revised gain versus frozen is concentrated in conversation41 (+11); the other three conversations sum to-8/391 (-2.05 percentage points). This is a descriptive subset calculation, not a population significance test. Four fixed DEV conversations and one same-model judge pass do not establish a generalizable advantage.

Successful reported usage. Cached input is already included in input totals. All reasoning output counts are zero. Estimates use the frozen credit schedule:2.5 uncached-input,0.25 cached-input and12.5 output credits per million tokens.

| System / phase | Input tokens | Cached input | Output tokens | Estimated credits |
|---|---:|---:|---:|---:|
| baseline / answers | 10,805,582 | 5,744,896 | 17,974 | 14.3126140 |
| baseline / judge_successful543 | 6,121,604 | 5,669,120 | 8,688 | 2.6570900 |
| candidate / answers | 10,806,949 | 5,958,912 | 17,854 | 13.8329955 |
| candidate / judge_successful543 | 6,121,581 | 5,691,136 | 8,688 | 2.6074965 |
| Mem0 / answers | 13,278,768 | 5,812,992 | 16,255 | 20.3208755 |
| Mem0 / judge_successful543 | 6,120,392 | 5,620,992 | 8,688 | 2.7623480 |
| Mem0 / extraction | 2,442,256 | 833,280 | 35,354 | 4.6726850 |

| System | Successful input | Successful cached input | Successful output | Known successful credits |
|---|---:|---:|---:|---:|
| baseline | 16,927,186 | 11,414,016 | 26,662 | 16.9697040 |
| candidate | 16,928,530 | 11,650,048 | 26,542 | 16.4404920 |
| Mem0 | 21,841,416 | 12,267,264 | 60,297 | 27.7559085 |

Known successful totals exclude the two failed frozen answer calls, one failed revised answer call and one failed Mem0 judge call; all four retained usage/charges are unknown. They are not zero. Credits are estimates from reported usage, not verified invoices, monetary ceilings, or total machine costs. Mem0 includes117 model extraction calls; HeurChain uses zero model extraction calls. Native CPU embedding work has no monetary estimate here.

Candidate attempt accounting:702 answer claims+543 judge claims=1245. Mem0:117 extraction+701 answer+544 judge=1362. Frozen:703 answer+543 judge=1246. Native sources2019turns/117batches, same701DEVquestions, same16384UTF8-byte complete-unit evidence cap and mandatory Codex harness; source dates and metadata count in the HeurChain whole units. Six heldout conversations stay sealed.

Candidate terminal at2026-10-07T05:12:31.064348+00:00. Per-call owned cleanup was reverified, and fresh candidate native inspection at2026-10-07T05:19:26.671145+00:00 confirmed inactive owned service, zero owned cgroup processes, no owned listening ports and absent owner PIDs. No new inference/native/embedding/network call was used in the three-way audit.
