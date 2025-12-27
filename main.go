package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	docPageSize = 20
)

type mode int

const (
	modeIndices mode = iota
	modeDocs
	modeQuery
	modeCreateDoc
	modeConfirmDelete
	modeDocDetails
)

type indexItem struct {
	info IndexInfo
}

type docItem struct {
	id      string
	preview string
	full    string
}

func (i indexItem) Title() string {
	return fmt.Sprintf("%s (%d docs)", i.info.Name, i.info.DocsCount)
}

func (i indexItem) Description() string {
	size := humanBytes(i.info.StoreBytes)
	if size == "0 B" {
		size = strings.TrimSpace(i.info.StoreSize)
		if size == "" {
			size = "n/a"
		}
	}
	return fmt.Sprintf(
		"health=%s status=%s size=%s",
		i.info.Health,
		i.info.Status,
		size,
	)
}

func (i indexItem) FilterValue() string {
	return i.info.Name
}

func (doc docItem) Title() string {
	if doc.id == "" {
		return "<generated id>"
	}
	return doc.id
}

func (doc docItem) Description() string {
	return doc.preview
}

func (doc docItem) FilterValue() string {
	return doc.id + doc.preview
}

type indicesLoadedMsg struct {
	items []list.Item
	err   error
}

type docsLoadedMsg struct {
	index  string
	query  string
	took   time.Duration
	items  []list.Item
	err    error
	fields []string
}

type docCreatedMsg struct {
	id  string
	err error
}

type docDeletedMsg struct {
	id  string
	err error
}

type fieldsLoadedMsg struct {
	fields []string
	err    error
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true)
	statusStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	queryHelp     = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render("Use Elasticsearch query_string syntax (blank => match_all)")
	queryExamples = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render(
		"Examples: status:200, host:api* AND duration:[0 TO 50], (error OR warning) AND service:web",
	)
	jsonKeyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("75"))
	jsonStringStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	jsonNumberStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	jsonBoolStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	jsonNullStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
)

type model struct {
	client *Client

	mode          mode
	ready         bool
	statusMessage string
	errMessage    string

	indexList list.Model
	docList   list.Model

	currentIndex string
	currentQuery string

	queryInput      textinput.Model
	docIDInput      textinput.Model
	docBodyInput    textarea.Model
	createStep      int
	pendingDelete   docItem
	detailDoc       docItem
	availableFields []string
	detailViewport  viewport.Model
}

func newModel(client *Client) model {
	indexList := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	indexList.Title = "Indices"
	indexList.SetShowStatusBar(false)
	indexList.SetFilteringEnabled(false)

	docList := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	docList.Title = "Documents"
	docList.SetShowStatusBar(false)
	docList.SetFilteringEnabled(false)

	queryInput := textinput.New()
	queryInput.Placeholder = "Query string (empty => match_all)"

	docIDInput := textinput.New()
	docIDInput.Placeholder = "Document ID (leave blank for auto)"

	docBody := textarea.New()
	docBody.SetWidth(60)
	docBody.SetHeight(10)
	docBody.Placeholder = `{"field":"value"}`
	docBody.ShowLineNumbers = false

	detailViewport := viewport.New(0, 0)
	detailViewport.MouseWheelEnabled = false

	return model{
		client:         client,
		mode:           modeIndices,
		indexList:      indexList,
		docList:        docList,
		queryInput:     queryInput,
		docIDInput:     docIDInput,
		docBodyInput:   docBody,
		detailViewport: detailViewport,
	}
}

func (m model) Init() tea.Cmd {
	return loadIndicesCmd(m.client)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		h := msg.Height - 2
		if h < 5 {
			h = msg.Height
		}
		m.indexList.SetSize(msg.Width, h)
		m.docList.SetSize(msg.Width, h)
		m.docBodyInput.SetWidth(msg.Width - 4)
		m.queryInput.Width = msg.Width - 4
		detailHeight := msg.Height - 4
		if detailHeight < 3 {
			detailHeight = msg.Height - 1
			if detailHeight < 1 {
				detailHeight = msg.Height
			}
		}
		m.detailViewport.Width = msg.Width
		m.detailViewport.Height = detailHeight
		m.ready = true
		return m, nil

	case indicesLoadedMsg:
		if msg.err != nil {
			m.errMessage = msg.err.Error()
			return m, nil
		}
		m.indexList.SetItems(msg.items)
		if len(msg.items) == 0 {
			m.statusMessage = "No indices found"
		} else {
			m.statusMessage = fmt.Sprintf("Loaded %d indices", len(msg.items))
		}
		return m, nil

	case docsLoadedMsg:
		if msg.err != nil {
			m.errMessage = msg.err.Error()
			return m, nil
		}
		if msg.index == m.currentIndex {
			m.docList.SetItems(msg.items)
			m.availableFields = mergeFields(m.availableFields, msg.fields)
			if len(msg.items) == 0 {
				m.statusMessage = fmt.Sprintf("%s: no docs (query: %s)", msg.index, emptyPlaceholder(msg.query))
			} else {
				m.statusMessage = fmt.Sprintf("%s: %d docs • %s • query=%s", msg.index, len(msg.items), msg.took, emptyPlaceholder(msg.query))
			}
		}
		return m, nil

	case fieldsLoadedMsg:
		if msg.err != nil {
			m.errMessage = msg.err.Error()
			return m, nil
		}
		m.availableFields = mergeFields(m.availableFields, msg.fields)
		return m, nil

	case docCreatedMsg:
		if msg.err != nil {
			m.errMessage = msg.err.Error()
		} else {
			m.statusMessage = fmt.Sprintf("Document %s indexed", msg.id)
		}
		m.mode = modeDocs
		return m, tea.Batch(loadDocsCmd(m.client, m.currentIndex, m.currentQuery), loadFieldsCmd(m.client, m.currentIndex))

	case docDeletedMsg:
		if msg.err != nil {
			m.errMessage = msg.err.Error()
		} else {
			m.statusMessage = fmt.Sprintf("Document %s deleted", msg.id)
		}
		m.mode = modeDocs
		return m, tea.Batch(loadDocsCmd(m.client, m.currentIndex, m.currentQuery), loadFieldsCmd(m.client, m.currentIndex))
	}

	switch m.mode {
	case modeIndices:
		return m.updateIndices(msg)
	case modeDocs:
		return m.updateDocs(msg)
	case modeQuery:
		return m.updateQueryInput(msg)
	case modeCreateDoc:
		return m.updateCreateDoc(msg)
	case modeConfirmDelete:
		return m.updateConfirmDelete(msg)
	case modeDocDetails:
		return m.updateDocDetails(msg)
	default:
		return m, nil
	}
}

func (m model) updateIndices(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.indexList, cmd = m.indexList.Update(msg)

	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "r":
			m.statusMessage = "Refreshing indices..."
			return m, tea.Batch(cmd, loadIndicesCmd(m.client))
		case "enter":
			item, ok := m.indexList.SelectedItem().(indexItem)
			if ok {
				m.currentIndex = item.info.Name
				m.currentQuery = ""
				m.queryInput.SetValue("")
				m.mode = modeDocs
				m.availableFields = nil
				m.statusMessage = fmt.Sprintf("Loading docs for %s...", m.currentIndex)
				return m, tea.Batch(cmd, loadDocsCmd(m.client, m.currentIndex, m.currentQuery), loadFieldsCmd(m.client, m.currentIndex))
			}
		}
	}
	return m, cmd
}

func (m model) updateDocs(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q", "esc":
			m.mode = modeIndices
			m.statusMessage = "Back to indices"
			return m, nil
		case "r":
			m.statusMessage = fmt.Sprintf("Refreshing %s", m.currentIndex)
			return m, tea.Batch(loadDocsCmd(m.client, m.currentIndex, m.currentQuery), loadFieldsCmd(m.client, m.currentIndex))
		case "/":
			m.mode = modeQuery
			m.queryInput.SetValue(m.currentQuery)
			m.queryInput.CursorEnd()
			m.queryInput.Focus()
			return m, nil
		case "n":
			m.mode = modeCreateDoc
			m.createStep = 0
			m.docIDInput.SetValue("")
			m.docIDInput.CursorStart()
			m.docBodyInput.SetValue("{\n  \"field\": \"value\"\n}")
			m.docBodyInput.Reset()
			return m, nil
		case "x", "delete":
			doc, ok := m.docList.SelectedItem().(docItem)
			if ok {
				m.mode = modeConfirmDelete
				m.pendingDelete = doc
				m.statusMessage = fmt.Sprintf("Delete %s? (y/N)", doc.id)
			}
			return m, nil
		case "enter", "v":
			doc, ok := m.docList.SelectedItem().(docItem)
			if ok {
				m.mode = modeDocDetails
				m.detailDoc = doc
				m.detailViewport.SetContent(doc.full)
				m.detailViewport.GotoTop()
				m.statusMessage = fmt.Sprintf("Viewing %s", displayDocTitle(doc.id))
			}
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.docList, cmd = m.docList.Update(msg)
	return m, cmd
}

func (m model) updateQueryInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.queryInput, cmd = m.queryInput.Update(msg)

	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.Type {
		case tea.KeyEnter:
			m.currentQuery = strings.TrimSpace(m.queryInput.Value())
			m.mode = modeDocs
			m.queryInput.Blur()
			m.statusMessage = fmt.Sprintf("Searching %s...", m.currentIndex)
			return m, tea.Batch(cmd, loadDocsCmd(m.client, m.currentIndex, m.currentQuery))
		case tea.KeyEsc:
			m.mode = modeDocs
			m.queryInput.Blur()
			return m, nil
		}
	}

	return m, cmd
}

func (m model) updateCreateDoc(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.Type {
		case tea.KeyEsc:
			m.mode = modeDocs
			return m, nil
		case tea.KeyEnter:
			if m.createStep == 0 {
				m.createStep = 1
				m.docBodyInput.Focus()
				return m, nil
			}
			body := strings.TrimSpace(m.docBodyInput.Value())
			id := strings.TrimSpace(m.docIDInput.Value())
			m.statusMessage = "Creating document..."
			return m, tea.Batch(createDocCmd(m.client, m.currentIndex, id, body))
		}
	}

	if m.createStep == 0 {
		var inputCmd tea.Cmd
		m.docIDInput, inputCmd = m.docIDInput.Update(msg)
		return m, inputCmd
	}

	var bodyCmd tea.Cmd
	m.docBodyInput, bodyCmd = m.docBodyInput.Update(msg)
	return m, bodyCmd
}

func (m model) updateConfirmDelete(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch strings.ToLower(keyMsg.String()) {
		case "y":
			m.mode = modeDocs
			m.statusMessage = fmt.Sprintf("Deleting %s...", m.pendingDelete.id)
			return m, deleteDocCmd(m.client, m.currentIndex, m.pendingDelete.id)
		case "n", "esc", "enter":
			m.mode = modeDocs
			m.statusMessage = "Delete canceled"
			return m, nil
		}
	}
	return m, nil
}

func (m model) updateDocDetails(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "esc", "q", "enter", "v":
			m.mode = modeDocs
			m.statusMessage = fmt.Sprintf("Back to %s", m.currentIndex)
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.detailViewport, cmd = m.detailViewport.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if !m.ready {
		return "Loading...\n"
	}

	var builder strings.Builder
	switch m.mode {
	case modeIndices:
		builder.WriteString(m.indexList.View())
	case modeDocs:
		builder.WriteString(titleStyle.Render(fmt.Sprintf("Index: %s | query=%s", m.currentIndex, emptyPlaceholder(m.currentQuery))))
		builder.WriteRune('\n')
		builder.WriteString(m.docList.View())
	case modeQuery:
		builder.WriteString("Enter search query:\n")
		builder.WriteString(m.queryInput.View())
		builder.WriteRune('\n')
		builder.WriteString(queryHelp)
		builder.WriteRune('\n')
		builder.WriteString(queryExamples)
		if fieldsLine := renderFieldList(m.availableFields); fieldsLine != "" {
			builder.WriteRune('\n')
			builder.WriteString(fieldsLine)
		}
	case modeCreateDoc:
		builder.WriteString(titleStyle.Render("Create Document"))
		builder.WriteRune('\n')
		if m.createStep == 0 {
			builder.WriteString("Document ID (blank => auto):\n")
			builder.WriteString(m.docIDInput.View())
		} else {
			builder.WriteString("Document body (compact JSON):\n")
			builder.WriteString(m.docBodyInput.View())
			builder.WriteString("\nPress Enter to submit")
		}
	case modeConfirmDelete:
		builder.WriteString(titleStyle.Render("Confirm delete"))
		builder.WriteRune('\n')
		builder.WriteString(fmt.Sprintf("Delete document %s? (y/N)", m.pendingDelete.id))
	case modeDocDetails:
		builder.WriteString(titleStyle.Render(fmt.Sprintf("Document %s", displayDocTitle(m.detailDoc.id))))
		builder.WriteRune('\n')
		builder.WriteString(m.detailViewport.View())
		builder.WriteString("\n(esc/q/enter to go back)")
	}

	builder.WriteRune('\n')
	builder.WriteString(renderStatus(m))
	return builder.String()
}

func renderStatus(m model) string {
	help := "q:quit r:refresh enter:open /:query n:new doc x:delete"
	switch m.mode {
	case modeIndices:
		help = "enter:open index r:refresh q:quit"
	case modeDocs:
		help = "esc:back r:refresh /:query n:new x:delete enter:view q:quit"
	case modeQuery:
		help = "enter:run esc:cancel"
	case modeCreateDoc:
		if m.createStep == 0 {
			help = "enter:next esc:cancel"
		} else {
			help = "enter:create esc:cancel"
		}
	case modeConfirmDelete:
		help = "y:confirm n:cancel"
	case modeDocDetails:
		help = "esc/q:back arrows/jk:scroll"
	}

	var parts []string
	if m.statusMessage != "" {
		parts = append(parts, statusStyle.Render(m.statusMessage))
	}
	if m.errMessage != "" {
		parts = append(parts, errorStyle.Render(m.errMessage))
	}
	parts = append(parts, help)
	return strings.Join(parts, " | ")
}

func emptyPlaceholder(v string) string {
	if strings.TrimSpace(v) == "" {
		return "match_all"
	}
	return v
}

func loadIndicesCmd(client *Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		indices, err := client.ListIndices(ctx)
		if err != nil {
			return indicesLoadedMsg{err: err}
		}
		items := make([]list.Item, 0, len(indices))
		for _, info := range indices {
			items = append(items, indexItem{info: info})
		}
		return indicesLoadedMsg{items: items}
	}
}

func loadDocsCmd(client *Client, index, query string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := client.Search(ctx, index, query, docPageSize)
		if err != nil {
			return docsLoadedMsg{index: index, query: query, err: err}
		}
		items := make([]list.Item, 0, len(res.Documents))
		fieldSet := make(map[string]struct{})
		for _, doc := range res.Documents {
			full := formatFullJSON(doc.Source)
			preview := previewCompactJSON(doc.Source, 160)
			items = append(items, docItem{id: doc.ID, preview: preview, full: full})
			collectFields(doc.Source, "", fieldSet)
		}
		fields := make([]string, 0, len(fieldSet))
		for field := range fieldSet {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		return docsLoadedMsg{index: index, query: query, took: res.Took, items: items, fields: fields}
	}
}

func loadFieldsCmd(client *Client, index string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		fields, err := client.ListFields(ctx, index)
		if err != nil {
			return fieldsLoadedMsg{err: err}
		}
		return fieldsLoadedMsg{fields: fields}
	}
}

func formatFullJSON(data map[string]any) string {
	if len(data) == 0 {
		return "(no _source)"
	}
	var builder strings.Builder
	renderJSONValue(&builder, data, 0)
	return builder.String()
}

func renderJSONValue(builder *strings.Builder, value any, indent int) {
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			builder.WriteString("{}")
			return
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		builder.WriteString("{\n")
		for i, key := range keys {
			builder.WriteString(strings.Repeat("  ", indent+1))
			builder.WriteString(jsonKeyStyle.Render(fmt.Sprintf("\"%s\"", escapeJSONString(key))))
			builder.WriteString(": ")
			renderJSONValue(builder, v[key], indent+1)
			if i < len(keys)-1 {
				builder.WriteString(",")
			}
			builder.WriteString("\n")
		}
		builder.WriteString(strings.Repeat("  ", indent) + "}")
	case []any:
		if len(v) == 0 {
			builder.WriteString("[]")
			return
		}
		builder.WriteString("[\n")
		for i, item := range v {
			builder.WriteString(strings.Repeat("  ", indent+1))
			renderJSONValue(builder, item, indent+1)
			if i < len(v)-1 {
				builder.WriteString(",")
			}
			builder.WriteString("\n")
		}
		builder.WriteString(strings.Repeat("  ", indent) + "]")
	case string:
		builder.WriteString(jsonStringStyle.Render(fmt.Sprintf("\"%s\"", escapeJSONString(v))))
	case float64:
		builder.WriteString(jsonNumberStyle.Render(strconv.FormatFloat(v, 'f', -1, 64)))
	case int, int64, int32:
		builder.WriteString(jsonNumberStyle.Render(fmt.Sprintf("%v", v)))
	case bool:
		builder.WriteString(jsonBoolStyle.Render(strconv.FormatBool(v)))
	case nil:
		builder.WriteString(jsonNullStyle.Render("null"))
	default:
		builder.WriteString(jsonStringStyle.Render(fmt.Sprintf("\"%v\"", v)))
	}
}

func escapeJSONString(value string) string {
	quoted := strconv.Quote(value)
	return quoted[1 : len(quoted)-1]
}

func previewCompactJSON(data map[string]any, maxLen int) string {
	if len(data) == 0 {
		return "(no _source)"
	}
	raw, err := json.Marshal(data)
	if err != nil {
		raw, _ = json.MarshalIndent(data, "", "  ")
	}
	return truncateString(string(raw), maxLen)
}

func truncateString(value string, maxLen int) string {
	if maxLen <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxLen {
		return value
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

func displayDocTitle(id string) string {
	if strings.TrimSpace(id) == "" {
		return "<generated id>"
	}
	return id
}

const maxFieldsDisplay = 25

func collectFields(data any, prefix string, out map[string]struct{}) {
	switch v := data.(type) {
	case map[string]any:
		for key, val := range v {
			field := key
			if prefix != "" {
				field = prefix + "." + key
			}
			out[field] = struct{}{}
			collectFields(val, field, out)
		}
	case []any:
		for _, item := range v {
			collectFields(item, prefix, out)
		}
	}
}

func renderFieldList(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	display := fields
	truncated := false
	if len(fields) > maxFieldsDisplay {
		display = fields[:maxFieldsDisplay]
		truncated = true
	}
	text := "Fields: " + strings.Join(display, ", ")
	if truncated {
		text += fmt.Sprintf(" … (+%d more)", len(fields)-maxFieldsDisplay)
	}
	return text
}

func mergeFields(current, incoming []string) []string {
	if len(incoming) == 0 {
		return current
	}
	set := make(map[string]struct{}, len(current)+len(incoming))
	for _, field := range current {
		if field == "" {
			continue
		}
		set[field] = struct{}{}
	}
	for _, field := range incoming {
		if field == "" {
			continue
		}
		set[field] = struct{}{}
	}
	merged := make([]string, 0, len(set))
	for field := range set {
		merged = append(merged, field)
	}
	sort.Strings(merged)
	return merged
}

func humanBytes(value int64) string {
	if value <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	val := float64(value)
	i := 0
	for val >= 1024 && i < len(units)-1 {
		val /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", value, units[i])
	}
	return fmt.Sprintf("%.2f %s", val, units[i])
}

func createDocCmd(client *Client, index, id, body string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		newID, err := client.CreateDoc(ctx, index, id, []byte(body))
		if err == nil {
			_ = client.Refresh(ctx, index)
		}
		return docCreatedMsg{id: newID, err: err}
	}
}

func deleteDocCmd(client *Client, index, id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := client.DeleteDoc(ctx, index, id)
		if err == nil {
			_ = client.Refresh(ctx, index)
		}
		return docDeletedMsg{id: id, err: err}
	}
}

func main() {
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	showHelp := fs.Bool("help", false, "Show help text")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags]\n", os.Args[0])
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Environment variables:")
		fmt.Fprintln(os.Stderr, "  ELASTICSEARCH_URL           Default http://localhost:9200")
		fmt.Fprintln(os.Stderr, "  ELASTICSEARCH_USERNAME/PASSWORD for basic auth")
		fmt.Fprintln(os.Stderr, "  ELASTICSEARCH_API_KEY       overrides basic auth when set")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Flags:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing flags: %v\n", err)
		os.Exit(2)
	}

	if *showHelp {
		fs.Usage()
		return
	}

	client, err := NewClientFromEnv()
	if err != nil {
		log.Fatalf("cannot init elasticsearch client: %v", err)
	}

	p := tea.NewProgram(newModel(client), tea.WithAltScreen())
	if err := p.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
