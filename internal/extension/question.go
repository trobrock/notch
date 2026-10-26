package extension

import (
	"context"
	"errors"
)

// QuestionHost is optional; hosts without it keep the Select/Input fallback.
// A capable host can still return ErrStructuredQuestionUnsupported until its
// client explicitly negotiates the structured question protocol.
var ErrStructuredQuestionUnsupported = errors.New("structured questions are not negotiated")

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}
type Question struct {
	Prompt              string           `json:"prompt"`
	Options             []QuestionOption `json:"options"`
	AllowCustomResponse bool             `json:"allowCustomResponse"`
	Placeholder         string           `json:"placeholder,omitempty"`
}
type QuestionAnswer struct {
	Value     string
	WasCustom bool
}
type QuestionHost interface {
	Question(context.Context, Question) (QuestionAnswer, error)
}
