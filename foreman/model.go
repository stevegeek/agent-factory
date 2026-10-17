package main

import (
	_ "embed"
	"encoding/json"
)

// The board's model comes from the dispatcher: `factory model > foreman/model.json`. A Python test checks the
// file is current; a Go test checks Foreman has a label and answers for everything in it.
//
//go:embed model.json
var modelJSON []byte

type Model struct {
	Columns        []string          `json:"columns"`
	ColumnLabels   map[string]string `json:"column_labels"`
	Stages         []string          `json:"stages"`
	StageLabels    map[string]string `json:"stage_labels"`
	StageForColumn map[string]string `json:"stage_for_column"`
	RunningColumn  map[string]string `json:"running_column"`
	AskKinds       []string          `json:"ask_kinds"`
	// board projects (mode "board"): their own columns, kinds, the API's capabilities, typed history
	Modes             []string            `json:"modes"`
	BoardColumns      []string            `json:"board_columns"`
	BoardColumnLabels map[string]string   `json:"board_column_labels"`
	Kinds             map[string][]string `json:"kinds"`
	Capabilities      []string            `json:"capabilities"`
	MoveReasons       []string            `json:"move_reasons"`
	EventTypes        map[string][]string `json:"event_types"`
}

var model = loadModel()

func loadModel() Model {
	var m Model
	if err := json.Unmarshal(modelJSON, &m); err != nil {
		panic("foreman/model.json: " + err.Error())
	}
	columns, roles = m.Columns, m.StageLabels
	// one lookup for any column: a board ticket's too (done is in both, with one label)
	columnLabels = map[string]string{}
	for k, v := range m.BoardColumnLabels {
		columnLabels[k] = v
	}
	for k, v := range m.ColumnLabels {
		columnLabels[k] = v
	}
	return m
}

// apiCapabilities: what an API token may be granted (Foreman's API checks them from step 2 of the ticket plan).
var apiCapabilities = []string{"read", "file", "talk", "work", "plan"}

// typedEvents: the typed history Foreman reads (events.go and its callers).
var typedEvents = []string{"created", "moved", "started", "finished", "memory", "noted", "asked", "answered"}
