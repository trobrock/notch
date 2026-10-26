package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/extension"
)

func TestLegacyInputCancellationAllowsEditorValue(t *testing.T) {
	for _, kind := range []string{"input", "select"} {
		t.Run(kind, func(t *testing.T) {
			var output bytes.Buffer
			server := New(strings.NewReader(""), &output, "/work")
			pending := &pendingInput{ctx: context.Background(), kind: kind, options: []string{"Yes", "No"}, reply: make(chan inputResponse, 1)}
			server.inputs = map[string]*pendingInput{"old-client-request": pending}
			if err := server.handleInputResponse(command{Type: "input_response", RequestID: "old-client-request", Cancelled: true, Value: "unfinished editor text"}); err != nil {
				t.Fatal(err)
			}
			var response response
			if err := json.Unmarshal(output.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if !response.Success || len(server.inputs) != 0 {
				t.Fatalf("legacy cancellation rejected: %s", output.String())
			}
			select {
			case answer := <-pending.reply:
				if !errors.Is(answer.err, context.Canceled) {
					t.Fatalf("cancellation result: %+v", answer)
				}
			default:
				t.Fatal("cancelled legacy input is still waiting")
			}
		})
	}
}

func TestStructuredFreeFormQuestionEmitsEmptyOptionsArray(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	server := New(strings.NewReader(""), writer, "/work")
	server.runner = &agent.Agent{}
	server.registry = extension.NewRegistry()
	server.structuredQuestions = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := server.Question(ctx, extension.Question{Prompt: "What next?", AllowCustomResponse: true})
		done <- err
	}()
	line, err := bufio.NewReader(reader).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(line, &event); err != nil {
		t.Fatal(err)
	}
	if string(event["options"]) != "[]" {
		t.Errorf("options = %s, want []", event["options"])
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("question cancellation = %v", err)
	}
}
