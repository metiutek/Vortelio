package server

import (
	"fmt"
	"strings"
)

// unifiedDiff returns a unified diff (3 lines of context) between two texts,
// plus the number of added and removed lines. Hunk bodies carry the old/new line
// numbers so terminals can render them without re-parsing headers.
func unifiedDiff(path, a, b string) (string, int, int) {
	if a == b {
		return "", 0, 0
	}
	al := splitDiffLines(a)
	bl := splitDiffLines(b)

	// Trim the common prefix/suffix first: edits are usually small, which keeps
	// the LCS table tiny even for big files.
	pre := 0
	for pre < len(al) && pre < len(bl) && al[pre] == bl[pre] {
		pre++
	}
	suf := 0
	for suf < len(al)-pre && suf < len(bl)-pre && al[len(al)-1-suf] == bl[len(bl)-1-suf] {
		suf++
	}
	am, bm := al[pre:len(al)-suf], bl[pre:len(bl)-suf]

	type op struct {
		kind byte // ' ', '-', '+'
		text string
	}
	var ops []op
	for _, l := range al[:pre] {
		ops = append(ops, op{' ', l})
	}
	if len(am)*len(bm) > 4_000_000 {
		// Too big for LCS: show it as a full replacement of the middle.
		for _, l := range am {
			ops = append(ops, op{'-', l})
		}
		for _, l := range bm {
			ops = append(ops, op{'+', l})
		}
	} else {
		n, m := len(am), len(bm)
		lcs := make([][]int32, n+1)
		for i := range lcs {
			lcs[i] = make([]int32, m+1)
		}
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if am[i] == bm[j] {
					lcs[i][j] = lcs[i+1][j+1] + 1
				} else if lcs[i+1][j] >= lcs[i][j+1] {
					lcs[i][j] = lcs[i+1][j]
				} else {
					lcs[i][j] = lcs[i][j+1]
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && am[i] == bm[j]:
				ops = append(ops, op{' ', am[i]})
				i++
				j++
			case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
				ops = append(ops, op{'-', am[i]})
				i++
			default:
				ops = append(ops, op{'+', bm[j]})
				j++
			}
		}
	}
	for _, l := range al[len(al)-suf:] {
		ops = append(ops, op{' ', l})
	}

	add, del := 0, 0
	for _, o := range ops {
		switch o.kind {
		case '+':
			add++
		case '-':
			del++
		}
	}

	// Group into hunks with 3 lines of context.
	const ctx = 3
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", path, path)
	oldNo := make([]int, len(ops))
	newNo := make([]int, len(ops))
	o, nn := 1, 1
	for k, op := range ops {
		oldNo[k], newNo[k] = o, nn
		if op.kind != '+' {
			o++
		}
		if op.kind != '-' {
			nn++
		}
	}
	k := 0
	for k < len(ops) {
		for k < len(ops) && ops[k].kind == ' ' {
			k++
		}
		if k >= len(ops) {
			break
		}
		start := k - ctx
		if start < 0 {
			start = 0
		}
		end := k
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			// run of context: stop if it is longer than 2*ctx
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*ctx {
				end += ctx
				if end > len(ops) {
					end = len(ops)
				}
				break
			}
			end = run
		}
		oc, nc := 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				oc++
			}
			if op.kind != '-' {
				nc++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldNo[start], oc, newNo[start], nc)
		for _, op := range ops[start:end] {
			out.WriteByte(op.kind)
			out.WriteString(op.text)
			out.WriteByte('\n')
		}
		k = end
	}
	return out.String(), add, del
}

func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
