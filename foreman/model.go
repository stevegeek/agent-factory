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
}

var model = loadModel()

func loadModel() Model {
	var m Model
	if err := json.Unmarshal(modelJSON, &m); err != nil {
		panic("foreman/model.json: " + err.Error())
	}
	columns, roles, columnLabels = m.Columns, m.StageLabels, m.ColumnLabels
	return m
}
