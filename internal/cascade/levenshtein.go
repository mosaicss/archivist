package cascade

import "strings"

// levenshtein returns the edit distance between two strings (case-folded).
// Only the distance is computed; no ranking needed beyond the <= 2 threshold.
func levenshtein(a, b string) int {
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			if a[i-1] == b[j-1] {
				curr[j] = prev[j-1]
			} else {
				m := prev[j-1]
				if prev[j] < m {
					m = prev[j]
				}
				if curr[j-1] < m {
					m = curr[j-1]
				}
				curr[j] = m + 1
			}
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

// nearMatch finds the closest filing type within edit distance <= 2.
// Returns empty string when no match is found.
func nearMatch(input string, candidates []string) string {
	best := ""
	bestDist := 3 // sentinel > 2
	for _, c := range candidates {
		d := levenshtein(input, c)
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	if bestDist <= 2 {
		return best
	}
	return ""
}
