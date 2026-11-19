package interactivity

import (
	"telegram-ollama-reply-bot/internal/systemone"
)

// RubricVersion identifies the question texts and criteria of the built-in
// rubric. Bump when the wording changes in a way that shifts score
// semantics; the ID is for operational diagnosis, not persisted state.
const RubricVersion = 1

// Stable question IDs of the built-in rubric (spec §4). They are keys of the
// System One request and never reach the model itself.
const (
	QuestionRelevance     = "relevance"
	QuestionIntrusion     = "intrusion"
	QuestionContribution  = "contribution"
	QuestionReactSuitable = "react_suitable"
	QuestionReplySuitable = "reply_suitable"
)

// rubricQuestions builds the five independent assessments of one decision
// request. The action-suitability questions are independent on purpose: a
// message can warrant both, one, or neither action, and an unsuitable reply
// must not read as evidence for a reaction.
func rubricQuestions() map[string]systemone.Question {
	return map[string]systemone.Question{
		QuestionRelevance: systemone.Score(
			"How relevant is the target message to the character's interests or role, given the conversation state? Judge only semantic relevance — not whether responding would be socially appropriate.",
			relevanceScoreLevels,
		),
		QuestionIntrusion: systemone.Noul(
			"Would an unsolicited intervention by the character at this moment interrupt, intrude, or be socially inappropriate? Answer yes or no.",
		),
		QuestionContribution: systemone.Noul(
			"Could the character add meaningful conversational value with a response at this moment? Answer yes or no.",
		),
		QuestionReactSuitable: systemone.Noul(
			"Would an emoji reaction by the character on the target message be natural and appropriate, even without any text response? Answer yes or no.",
		),
		QuestionReplySuitable: systemone.Noul(
			"Would an unsolicited full conversational reply by the character right now be natural and appropriate? Answer yes or no.",
		),
	}
}
