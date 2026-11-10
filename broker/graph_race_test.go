//go:build race

package main

// The race detector slows the serial graph walk (one SQLite query per node and
// one Redis read per edge, with miniredis in-process) by roughly 8-10x. On a
// 4-core development container a full 256-edge traversal measured about 345 ms
// under -race against the 300 ms budget, and about 40 ms without it; GOMAXPROCS
// 1, 2 and 4 made no difference. Widen the budget only in race test binaries so
// the budget tests check edge truncation, not host speed.
// Release builds never compile this file.
func init() { graphReadBudget = 10 * graphReadTimeout }
