package interactivity

import (
	"fmt"
	"math"

	"telegram-ollama-reply-bot/internal/systemone"
)

// DecisionScores are the five normalized semantic assessments of one
// decision, all finite within [0,1]. They are suitability signals, not
// calibrated probabilities of user approval.
type DecisionScores struct {
	// Relevance is the normalized ordinal interest score: the raw System
	// One score (a probability-weighted level mean over four levels) divided
	// by the maximum level, unrounded — the levels are criteria anchors,
	// not the output grid.
	Relevance float64
	// Intrusion is the probability that an unsolicited intervention would
	// interrupt, intrude, or be socially inappropriate.
	Intrusion float64
	// Contribution is the probability that the character could add
	// meaningful conversational value.
	Contribution float64
	// ReactSuitable is the probability that an emoji reaction alone would
	// be natural on this message.
	ReactSuitable float64
	// ReplySuitable is the probability that an unsolicited full reply would
	// be natural right now.
	ReplySuitable float64
}

// relevanceScoreLevels mirrors the spec §4 relevance scale: four ordered
// levels, lowest first, so the raw score spans [0,3].
var relevanceScoreLevels = []string{
	"Irrelevant to the character's interests or role",
	"Slightly relevant",
	"Relevant",
	"Highly relevant",
}

// relevanceMaxLevel is the highest raw score the relevance question can
// produce; the normalized value is raw / relevanceMaxLevel.
const relevanceMaxLevel = 3.0

// validScore reports whether a normalized score is usable evidence.
func validScore(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

// scoresFromAnswers maps the validated rubric answers to decision scores.
// The System One client already guarantees protocol shape and ranges; this
// adds the semantic mapping and a final finite/range defense, because the
// policy must treat any unusable evidence as invalid, never as zero.
func scoresFromAnswers(answers map[string]systemone.Answer) (DecisionScores, error) {
	relevance, ok := answers[QuestionRelevance]
	if !ok || relevance.Type != systemone.TypeScore {
		return DecisionScores{}, fmt.Errorf("%w: relevance answer missing or mistyped", ErrInvalidResponse)
	}
	noul := func(id string) (float64, error) {
		answer, ok := answers[id]
		if !ok || answer.Type != systemone.TypeNoul {
			return 0, fmt.Errorf("%w: %s answer missing or mistyped", ErrInvalidResponse, id)
		}

		return answer.Noul, nil
	}

	intrusion, err := noul(QuestionIntrusion)
	if err != nil {
		return DecisionScores{}, err
	}
	contribution, err := noul(QuestionContribution)
	if err != nil {
		return DecisionScores{}, err
	}
	reactSuitable, err := noul(QuestionReactSuitable)
	if err != nil {
		return DecisionScores{}, err
	}
	replySuitable, err := noul(QuestionReplySuitable)
	if err != nil {
		return DecisionScores{}, err
	}

	scores := DecisionScores{
		Relevance:     relevance.Score / relevanceMaxLevel,
		Intrusion:     intrusion,
		Contribution:  contribution,
		ReactSuitable: reactSuitable,
		ReplySuitable: replySuitable,
	}
	for name, value := range map[string]float64{
		"relevance":      scores.Relevance,
		"intrusion":      scores.Intrusion,
		"contribution":   scores.Contribution,
		"react_suitable": scores.ReactSuitable,
		"reply_suitable": scores.ReplySuitable,
	} {
		if !validScore(value) {
			return DecisionScores{}, fmt.Errorf("%w: %s score is not usable evidence", ErrInvalidResponse, name)
		}
	}

	return scores, nil
}
