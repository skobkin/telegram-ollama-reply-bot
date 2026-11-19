package interactivity

import (
	"errors"
	"testing"

	"telegram-ollama-reply-bot/internal/systemone"
)

// TestScoresFromAnswersNormalization pins the relevance normalization: raw
// probability-weighted mean divided by the maximum level, unrounded.
func TestScoresFromAnswersNormalization(t *testing.T) {
	answers := map[string]systemone.Answer{
		QuestionRelevance:     {Type: systemone.TypeScore, Score: 2.0858},
		QuestionIntrusion:     {Type: systemone.TypeNoul, Noul: 0.4},
		QuestionContribution:  {Type: systemone.TypeNoul, Noul: 0.7},
		QuestionReactSuitable: {Type: systemone.TypeNoul, Noul: 0.2},
		QuestionReplySuitable: {Type: systemone.TypeNoul, Noul: 0.9},
	}

	scores, err := scoresFromAnswers(answers)
	if err != nil {
		t.Fatalf("map answers: %v", err)
	}
	// Runtime arithmetic (a variable, not a folded constant) so the want
	// value matches the production computation bit for bit.
	raw := 2.0858
	want := raw / relevanceMaxLevel
	if scores.Relevance != want {
		t.Fatalf("relevance must be raw/%v unrounded, got %.17g want %.17g", relevanceMaxLevel, scores.Relevance, want)
	}
	if scores.Intrusion != 0.4 || scores.Contribution != 0.7 || scores.ReactSuitable != 0.2 || scores.ReplySuitable != 0.9 {
		t.Fatalf("noul probabilities must map directly: %+v", scores)
	}
}

// TestScoresFromAnswersRejectsUnusableEvidence pins the semantic mapping
// checks: missing, mistyped, or out-of-range answers are invalid evidence.
func TestScoresFromAnswersRejectsUnusableEvidence(t *testing.T) {
	valid := func() map[string]systemone.Answer {
		return map[string]systemone.Answer{
			QuestionRelevance:     {Type: systemone.TypeScore, Score: 1},
			QuestionIntrusion:     {Type: systemone.TypeNoul, Noul: 0},
			QuestionContribution:  {Type: systemone.TypeNoul, Noul: 0},
			QuestionReactSuitable: {Type: systemone.TypeNoul, Noul: 0},
			QuestionReplySuitable: {Type: systemone.TypeNoul, Noul: 0},
		}
	}

	cases := map[string]func(map[string]systemone.Answer){
		"missing relevance": func(a map[string]systemone.Answer) { delete(a, QuestionRelevance) },
		"relevance mistyped as noul": func(a map[string]systemone.Answer) {
			a[QuestionRelevance] = systemone.Answer{Type: systemone.TypeNoul, Noul: 1}
		},
		"missing intrusion": func(a map[string]systemone.Answer) { delete(a, QuestionIntrusion) },
		"intrusion mistyped as choice": func(a map[string]systemone.Answer) {
			a[QuestionIntrusion] = systemone.Answer{Type: systemone.TypeChoice, Choice: "x"}
		},
		"missing reply suitability": func(a map[string]systemone.Answer) { delete(a, QuestionReplySuitable) },
		"out-of-range contribution": func(a map[string]systemone.Answer) {
			a[QuestionContribution] = systemone.Answer{Type: systemone.TypeNoul, Noul: 1.5}
		},
		"score above the level maximum": func(a map[string]systemone.Answer) {
			a[QuestionRelevance] = systemone.Answer{Type: systemone.TypeScore, Score: 3.5}
		},
	}
	for name, mutate := range cases {
		answers := valid()
		mutate(answers)

		if _, err := scoresFromAnswers(answers); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("%s must map to ErrInvalidResponse, got %v", name, err)
		}
	}
}
