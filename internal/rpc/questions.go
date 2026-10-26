package rpc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/trobrock/notch/internal/extension"
)

const structuredQuestionsCapability = "structured_questions_v1"

func (s *Server) Question(ctx context.Context, question extension.Question) (extension.QuestionAnswer, error) {
	s.mu.Lock()
	enabled := s.structuredQuestions
	s.mu.Unlock()
	if !enabled {
		return extension.QuestionAnswer{}, extension.ErrStructuredQuestionUnsupported
	}
	if len(question.Options) > 100 || strings.TrimSpace(question.Prompt) == "" || len(question.Options) == 0 && !question.AllowCustomResponse {
		return extension.QuestionAnswer{}, errors.New("question has no prompt or allowed answers")
	}
	seen := map[string]bool{}
	for _, option := range question.Options {
		if strings.TrimSpace(option.Label) == "" || seen[option.Label] || len(option.Label) > 8000 || len(option.Description) > 16000 {
			return extension.QuestionAnswer{}, errors.New("question labels must be nonempty and unique")
		}
		seen[option.Label] = true
	}
	question.Options = append([]extension.QuestionOption{}, question.Options...)
	response, err := s.requestInteractiveInput(ctx, "question", question.Prompt, question.Placeholder, nil, &question)
	return extension.QuestionAnswer{Value: response.value, WasCustom: response.wasCustom}, err
}

func (s *Server) setCapabilities(request command) error {
	enabled := false
	for _, capability := range request.Capabilities {
		if capability != structuredQuestionsCapability {
			return s.failure(request.ID, request.Type, fmt.Errorf("unsupported client capability %q", capability))
		}
		enabled = true
	}
	s.mu.Lock()
	if s.active || s.compacting || s.commandActive || len(s.inputs) > 0 {
		s.mu.Unlock()
		return s.failure(request.ID, request.Type, errors.New("client capabilities can only change while idle"))
	}
	s.structuredQuestions = enabled
	s.mu.Unlock()
	capabilities := []string{}
	if enabled {
		capabilities = append(capabilities, structuredQuestionsCapability)
	}
	return s.success(request, map[string]any{"capabilities": capabilities})
}

func validateStructuredAnswer(pending *pendingInput, request command) error {
	if pending.kind != "question" {
		if request.WasCustom != nil {
			return errors.New("wasCustom is only valid for structured questions")
		}
		return nil
	}
	if request.Cancelled {
		if request.Value != "" || request.WasCustom != nil {
			return errors.New("cancelled input cannot contain an answer")
		}
		return nil
	}
	if request.WasCustom == nil {
		return errors.New("structured question response requires wasCustom")
	}
	if strings.TrimSpace(request.Value) == "" || len(request.Value) > 8000 {
		return errors.New("question answer cannot be blank")
	}
	if *request.WasCustom {
		if !pending.question.AllowCustomResponse {
			return errors.New("custom answers are not allowed")
		}
		return nil
	}
	for _, option := range pending.question.Options {
		if option.Label == request.Value {
			return nil
		}
	}
	return errors.New("question response must match an exact option label")
}
