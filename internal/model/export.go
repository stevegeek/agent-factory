package model

// Model is the JSON shape `factory model` prints (model()), built
// from the tables only. Printed with sorted keys and indent 2, it is byte
// for byte the old Foreman's model.json (internal/model/testdata).
type Model struct {
	Columns           []string            `json:"columns"`
	ColumnLabels      map[string]string   `json:"column_labels"`
	Stages            []string            `json:"stages"`
	StageLabels       map[string]string   `json:"stage_labels"`
	StageForColumn    map[string]string   `json:"stage_for_column"`
	RunningColumn     map[string]string   `json:"running_column"`
	AskKinds          []string            `json:"ask_kinds"`
	Modes             []string            `json:"modes"`
	BoardColumns      []string            `json:"board_columns"`
	BoardColumnLabels map[string]string   `json:"board_column_labels"`
	Kinds             map[string][]string `json:"kinds"`
	Capabilities      []string            `json:"capabilities"`
	MoveReasons       []string            `json:"move_reasons"`
	EventTypes        map[string][]string `json:"event_types"`
}

// Export is the model as `factory model` prints it. Every call builds new
// maps and slices.
func Export() Model {
	m := Model{
		ColumnLabels: map[string]string{}, StageLabels: map[string]string{}, StageForColumn: map[string]string{},
		RunningColumn: map[string]string{}, BoardColumnLabels: map[string]string{}, Kinds: map[string][]string{},
		EventTypes: map[string][]string{},
	}
	for _, c := range Columns() {
		m.Columns = append(m.Columns, string(c.Name))
		m.ColumnLabels[string(c.Name)] = c.Label
	}
	for _, s := range Stages() {
		m.Stages = append(m.Stages, string(s.Name))
		m.StageLabels[string(s.Name)] = s.Label
		m.RunningColumn[string(s.Name)] = string(s.RunningColumn)
		if s.WaitColumn != "" {
			m.StageForColumn[string(s.WaitColumn)] = string(s.Name)
		}
	}
	for _, k := range AskKinds() {
		m.AskKinds = append(m.AskKinds, string(k.Kind))
	}
	for _, mode := range Modes() {
		m.Modes = append(m.Modes, string(mode.Mode))
		kinds := []string{}
		for _, k := range mode.Kinds {
			kinds = append(kinds, string(k))
		}
		m.Kinds[string(mode.Mode)] = kinds
	}
	for _, c := range BoardColumns() {
		m.BoardColumns = append(m.BoardColumns, string(c.Name))
		m.BoardColumnLabels[string(c.Name)] = c.Label
	}
	for _, c := range Capabilities() {
		m.Capabilities = append(m.Capabilities, string(c))
	}
	for _, r := range Reasons() {
		m.MoveReasons = append(m.MoveReasons, string(r.Reason))
	}
	for _, e := range EventTypes() {
		m.EventTypes[string(e.Type)] = e.Fields
	}
	return m
}
