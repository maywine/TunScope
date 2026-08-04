package mactun

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestEngineControllerWaitsForMatchingAcknowledgement(t *testing.T) {
	commandRead, commandWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	responseRead, responseWrite, err := os.Pipe()
	if err != nil {
		commandRead.Close()
		commandWrite.Close()
		t.Fatal(err)
	}
	defer commandRead.Close()
	defer responseWrite.Close()
	controller := newEngineController(commandWrite, responseRead)
	defer controller.Close()

	go func() {
		var command EngineControlCommand
		if err := json.NewDecoder(commandRead).Decode(&command); err != nil {
			return
		}
		_ = json.NewEncoder(responseWrite).Encode(EngineControlResponse{
			Action: command.Action, Generation: command.Generation, Closed: 7,
		})
	}()

	closed, err := controller.RebindNetwork("192.168.50.37", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 7 {
		t.Fatalf("acknowledged closed flows = %d, want 7", closed)
	}
}

func TestEngineControllerRejectsMismatchedAcknowledgement(t *testing.T) {
	commandRead, commandWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	responseRead, responseWrite, err := os.Pipe()
	if err != nil {
		commandRead.Close()
		commandWrite.Close()
		t.Fatal(err)
	}
	defer commandRead.Close()
	defer responseWrite.Close()
	controller := newEngineController(commandWrite, responseRead)
	defer controller.Close()

	go func() {
		var command EngineControlCommand
		if err := json.NewDecoder(commandRead).Decode(&command); err != nil {
			return
		}
		command.Generation++
		_ = json.NewEncoder(responseWrite).Encode(EngineControlResponse{
			Action: command.Action, Generation: command.Generation,
		})
	}()

	if _, err := controller.RebindNetwork("192.168.50.37", time.Second); err == nil {
		t.Fatal("expected mismatched acknowledgement error")
	}
}
