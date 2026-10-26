package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/extension"
)

func questionBool(value bool) *bool { return &value }
func TestStructuredQuestionRequiresClientOptIn(t *testing.T) {
	server := New(strings.NewReader(""), io.Discard, "/work")
	_, err := server.Question(context.Background(), extension.Question{Prompt: "Question?", AllowCustomResponse: true})
	if !errors.Is(err, extension.ErrStructuredQuestionUnsupported) {
		t.Fatal("legacy client received unnegotiated question")
	}
	if err := server.setCapabilities(command{Type: "set_capabilities", Capabilities: []string{structuredQuestionsCapability}}); err != nil || !server.structuredQuestions {
		t.Fatal("capability opt in failed")
	}
}
func TestStructuredQuestionRoundTripChoiceCustomCancellation(t *testing.T) {
	for _, mode := range []string{"choice", "custom", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			lines := bufio.NewReader(reader)
			server := New(strings.NewReader(""), writer, "/work")
			server.runner = &agent.Agent{}
			server.registry = extension.NewRegistry()
			server.structuredQuestions = true
			type outcome struct {
				answer extension.QuestionAnswer
				err    error
			}
			result := make(chan outcome, 1)
			go func() {
				answer, err := server.Question(context.Background(), extension.Question{Prompt: "Continue?", Options: []extension.QuestionOption{{Label: "Continue", Description: "Bounded work"}, {Label: "Type a custom response", Description: "Literal choice"}}, AllowCustomResponse: true, Placeholder: "Your limits"})
				result <- outcome{answer, err}
			}()
			line, err := lines.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var event struct {
				RequestID   string                     `json:"requestId"`
				Kind        string                     `json:"kind"`
				Options     []extension.QuestionOption `json:"options"`
				AllowCustom bool                       `json:"allowCustomResponse"`
				Placeholder string                     `json:"placeholder"`
			}
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatal(err)
			}
			if event.Kind != "question" || len(event.Options) != 2 || event.Options[0].Description != "Bounded work" || !event.AllowCustom || event.Placeholder != "Your limits" {
				t.Fatalf("lost structure: %+v", event)
			}
			// Invalid responses leave the original request pending.
			invalid := make(chan error, 1)
			go func() {
				invalid <- server.handleInputResponse(command{Type: "input_response", RequestID: event.RequestID, Value: "invented", WasCustom: questionBool(false)})
			}()
			line, err = lines.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var rejected response
			_ = json.Unmarshal(line, &rejected)
			if rejected.Success {
				t.Fatal("unknown choice accepted")
			}
			if err := <-invalid; err != nil {
				t.Fatal(err)
			}
			answer := command{Type: "input_response", RequestID: event.RequestID, Value: "Type a custom response", WasCustom: questionBool(false)}
			if mode == "custom" {
				answer.Value = "only ten minutes"
				answer.WasCustom = questionBool(true)
			}
			if mode == "cancel" {
				answer.Value = ""
				answer.WasCustom = nil
				answer.Cancelled = true
			}
			handled := make(chan error, 1)
			go func() { handled <- server.handleInputResponse(answer) }()
			if _, err := lines.ReadBytes('\n'); err != nil {
				t.Fatal(err)
			}
			if err := <-handled; err != nil {
				t.Fatal(err)
			}
			got := <-result
			if mode == "cancel" {
				if !errors.Is(got.err, context.Canceled) {
					t.Fatal("cancellation lost")
				}
			} else if got.err != nil || got.answer.Value != answer.Value || got.answer.WasCustom != (mode == "custom") {
				t.Fatalf("wrong outcome: %+v", got)
			}
			server.mu.Lock()
			remaining := len(server.inputs)
			server.mu.Unlock()
			if remaining != 0 {
				t.Fatal("custom answer required second request")
			}
		})
	}
}
func TestStructuredAnswerPolicyAndCancellationValidation(t *testing.T) {
	pending := &pendingInput{kind: "question", question: &extension.Question{Options: []extension.QuestionOption{{Label: "A"}}}}
	for _, answer := range []command{{Value: "A"}, {Value: "custom", WasCustom: questionBool(true)}, {Cancelled: true, Value: "fake answer"}, {Cancelled: true, WasCustom: questionBool(false)}} {
		if validateStructuredAnswer(pending, answer) == nil {
			t.Fatalf("invalid response accepted: %+v", answer)
		}
	}
	if err := validateStructuredAnswer(pending, command{Value: "A", WasCustom: questionBool(false)}); err != nil {
		t.Fatal(err)
	}
}

func TestSetCapabilitiesCommandIsDispatchedOnRPCWire(t *testing.T) {
	output := &strings.Builder{}
	server := New(strings.NewReader(""), output, "/work")
	server.runner = &agent.Agent{}
	server.registry = extension.NewRegistry()
	raw := []byte(`{"id":"question-protocol","type":"set_capabilities","capabilities":["structured_questions_v1"]}`)
	var request command
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if err := server.handle(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !server.structuredQuestions || !strings.Contains(output.String(), `"success":true`) {
		t.Fatalf("capability command not dispatched: %s", output)
	}
}

func TestOversizedQuestionAnswerDoesNotConsumePendingRequest(t *testing.T) {
	output := &strings.Builder{}
	server := New(strings.NewReader(""), output, "/work")
	pending := &pendingInput{ctx: context.Background(), kind: "question", question: &extension.Question{AllowCustomResponse: true}, reply: make(chan inputResponse, 1)}
	server.inputs = map[string]*pendingInput{"request": pending}
	if err := server.handleInputResponse(command{Type: "input_response", RequestID: "request", Value: strings.Repeat("x", 8001), WasCustom: questionBool(true)}); err != nil {
		t.Fatal(err)
	}
	if server.inputs["request"] != pending || !strings.Contains(output.String(), `"success":false`) {
		t.Fatal("oversized answer was accepted or pending request lost")
	}
}
