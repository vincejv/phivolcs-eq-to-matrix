package main

import (
	"regexp"
	"strings"
)

// basic replacements for common address tokens
var addrMap = map[string]string{
	"st": "street", "st.": "street",
	"rd": "road", "rd.": "road",
	"ave": "avenue", "ave.": "avenue",
	"blk": "block", "brgy": "barangay",
	"ph": "phase", "subd": "subdivision",
}

// Normalize address: lowercase, expand abbrev, remove punct/spaces
func normalizeAddr(s string) string {
	s = strings.ToLower(s)
	re := regexp.MustCompile(`[^\w\s]`)
	s = re.ReplaceAllString(s, " ")
	fields := strings.Fields(s)
	for i, f := range fields {
		if rep, ok := addrMap[f]; ok {
			fields[i] = rep
		}
	}
	return strings.Join(fields, "")
}

// Levenshtein distance
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	prev := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		cur := make([]int, lb+1)
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			del := prev[j] + 1
			ins := cur[j-1] + 1
			sub := prev[j-1] + cost
			cur[j] = min(del, ins, sub)
		}
		prev = cur
	}
	return prev[lb]
}

func min(a, b, c int) int {
	if a < b && a < c {
		return a
	}
	if b < c {
		return b
	}
	return c
}

func levenshteinPercent(a, b string) float64 {
	if a == b {
		return 100
	}
	dist := levenshtein(a, b)
	maxLen := float64(max(len(a), len(b)))
	if maxLen == 0 {
		return 100
	}
	return (1 - float64(dist)/maxLen) * 100
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func AddressSimilarity(a, b string) float64 {
	return levenshteinPercent(normalizeAddr(a), normalizeAddr(b))
}

// func main() {
// 	a1 := "Blk 5 Lot 3, Brgy San Jose, Cebu City"
// 	a2 := "Block 5 Lot 3 Barangay San Jose Cebu City"
// 	fmt.Printf("Similarity: %.2f%%\n", AddressSimilarity(a1, a2))
// }
