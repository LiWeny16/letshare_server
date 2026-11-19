package handler

import (
	"strings"
	"unicode"
)

// Meaningless recognizer fragments must not become transcript rows, sequence
// numbers, model requests, or a new live-summary timer.
func meaningfulMeetingASRText(text string) bool {
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	compact := strings.Join(words, "")
	runes := []rune(compact)
	if len(runes) <= 1 {
		return false
	}
	const fillers = "嗯啊呃额唔噢哦喔呣唉欸诶哼咳哈呵"
	for _, word := range words {
		allChineseFillers := true
		for _, r := range word {
			if !strings.ContainsRune(fillers, r) {
				allChineseFillers = false
				break
			}
		}
		if allChineseFillers {
			continue
		}
		switch strings.ToLower(word) {
		case "um", "umm", "uhm", "uh", "uhh", "huh", "hm", "hmm", "hmmm", "mm", "mmm", "mmhmm", "erm", "er", "ah", "eh", "mhm":
		default:
			return true
		}
	}
	return false
}
