package cli

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// hNode is the normalized, cross-driver view-hierarchy node. Drivers return
// platform-specific formats (Android UIAutomator XML, iOS WDA XML, or the
// devicelab flat-JSON snapshot); we normalize them all to this shape so the
// `hierarchy` command's output is consistent and pipe/diff-friendly.
type hNode struct {
	Type   string   `json:"type,omitempty"`
	ID     string   `json:"id,omitempty"`
	Text   string   `json:"text,omitempty"`
	Bounds *hBounds `json:"bounds,omitempty"`
	// Element states. Enabled is reported on every element whose driver
	// reports it, true or false, so a phone's tree reads like a simulator's;
	// Checked only for checkable elements; Selected/Focused only when true.
	Enabled  *bool `json:"enabled,omitempty"`
	Checked  *bool `json:"checked,omitempty"`
	Selected *bool `json:"selected,omitempty"`
	Focused  *bool `json:"focused,omitempty"`
	// Truncated is set on the root when the driver stopped listing elements
	// at a node cap (the devicelab iOS runner's snapshot limit), so the tree
	// is known to be incomplete.
	Truncated *bool   `json:"truncated,omitempty"`
	Children  []hNode `json:"children,omitempty"`
}

func boolPtr(b bool) *bool { return &b }

type hBounds struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// formatHierarchy normalizes a driver's raw hierarchy output and renders it as
// a JSON tree (default), a flat compact listing (compact), or a filtered flat
// listing of matching elements (find, case-insensitive substring).
func formatHierarchy(raw []byte, compact bool, find string) (string, error) {
	root, err := parseHierarchy(raw)
	if err != nil {
		return "", err
	}
	if find != "" {
		var b strings.Builder
		renderFind(root, strings.ToLower(find), &b)
		out := strings.TrimRight(b.String(), "\n")
		if out == "" {
			if root.Truncated != nil && *root.Truncated {
				return fmt.Sprintf("(no elements matching %q; the hierarchy was truncated at the driver's node cap, so some elements are not listed)", find), nil
			}
			return fmt.Sprintf("(no elements matching %q)", find), nil
		}
		return out, nil
	}
	if compact {
		var b strings.Builder
		renderCompact(root, 0, &b)
		return strings.TrimRight(b.String(), "\n"), nil
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseHierarchy detects the driver's output format and normalizes it.
func parseHierarchy(raw []byte) (hNode, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return hNode{}, nil
	}
	switch trimmed[0] {
	case '{':
		return parseJSON(trimmed)
	case '<':
		return parseXML(trimmed)
	default:
		return hNode{}, fmt.Errorf("unrecognized hierarchy format")
	}
}

// parseJSON handles both JSON shapes drivers emit: the devicelab flat
// {"nodes":[...]} snapshot, and an already-tree-shaped node
// ({"type":...,"children":[...]}, as the mock/web drivers return).
func parseJSON(raw []byte) (hNode, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return hNode{}, fmt.Errorf("parse hierarchy JSON: %w", err)
	}
	if _, ok := probe["nodes"]; ok {
		return parseFlatJSON(raw)
	}
	var node hNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return hNode{}, fmt.Errorf("parse hierarchy JSON tree: %w", err)
	}
	return node, nil
}

// ---- devicelab flat-JSON ({"nodes":[{index,type,identifier,label,value,rect,parentIndex}...]}) ----

// flatRect is a devicelab snapshot frame. XCUITest frames are in points and
// often fractional (y 700.67), so they are read as floats and rounded.
type flatRect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

func (r flatRect) bounds() *hBounds {
	return &hBounds{
		X:      int(math.Round(r.X)),
		Y:      int(math.Round(r.Y)),
		Width:  int(math.Round(r.Width)),
		Height: int(math.Round(r.Height)),
	}
}

func parseFlatJSON(raw []byte) (hNode, error) {
	var doc struct {
		Nodes []struct {
			Index       int      `json:"index"`
			Type        string   `json:"type"`
			Label       string   `json:"label"`
			Identifier  string   `json:"identifier"`
			Value       string   `json:"value"`
			Rect        flatRect `json:"rect"`
			Enabled     *bool    `json:"enabled"`
			Focused     *bool    `json:"focused"`
			Selected    *bool    `json:"selected"`
			ParentIndex *int     `json:"parentIndex"`
		} `json:"nodes"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return hNode{}, fmt.Errorf("parse hierarchy JSON: %w", err)
	}
	// Build nodes, then wire children by parentIndex. index is 0-based and
	// dense in the flat list, so position == index.
	built := make([]hNode, len(doc.Nodes))
	for i, n := range doc.Nodes {
		text := n.Label
		if text == "" {
			text = n.Value
		}
		node := hNode{Type: n.Type, ID: n.Identifier, Text: text, Bounds: n.Rect.bounds()}
		if n.Enabled != nil {
			node.Enabled = boolPtr(*n.Enabled)
		}
		if n.Focused != nil && *n.Focused {
			node.Focused = boolPtr(true)
		}
		if n.Selected != nil && *n.Selected {
			node.Selected = boolPtr(true)
		}
		built[i] = node
	}
	var roots []hNode
	// Attach children to parents. Because Go slices copy by value, assemble
	// bottom-up using indices into a parallel child-list.
	childIdx := make([][]int, len(doc.Nodes))
	rootIdx := []int{}
	for i, n := range doc.Nodes {
		if n.ParentIndex == nil || *n.ParentIndex < 0 || *n.ParentIndex >= len(doc.Nodes) || *n.ParentIndex == i {
			rootIdx = append(rootIdx, i)
		} else {
			p := *n.ParentIndex
			childIdx[p] = append(childIdx[p], i)
		}
	}
	var assemble func(i int) hNode
	assemble = func(i int) hNode {
		node := built[i]
		for _, c := range childIdx[i] {
			node.Children = append(node.Children, assemble(c))
		}
		return node
	}
	for _, r := range rootIdx {
		roots = append(roots, assemble(r))
	}
	root := hNode{Type: "Root", Children: roots}
	if len(roots) == 1 {
		root = roots[0]
	}
	if doc.Truncated {
		root.Truncated = boolPtr(true)
	}
	return root, nil
}

// ---- XML (Android UIAutomator + iOS WDA) ----

type rawXML struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Children []rawXML   `xml:",any"`
}

func (r rawXML) attr(name string) string {
	for _, a := range r.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func parseXML(raw []byte) (hNode, error) {
	var root rawXML
	if err := xml.Unmarshal(raw, &root); err != nil {
		return hNode{}, fmt.Errorf("parse hierarchy XML: %w", err)
	}
	isAndroid := strings.EqualFold(root.XMLName.Local, "hierarchy") || root.attr("resource-id") != "" || hasNodeChildren(root)
	// The Android <hierarchy> root is a wrapper, not a real element.
	if strings.EqualFold(root.XMLName.Local, "hierarchy") {
		converted := hNode{Type: "Root"}
		for _, c := range root.Children {
			converted.Children = append(converted.Children, convertXML(c, true))
		}
		if len(converted.Children) == 1 {
			return converted.Children[0], nil
		}
		return converted, nil
	}
	return convertXML(root, isAndroid), nil
}

func hasNodeChildren(r rawXML) bool {
	for _, c := range r.Children {
		if strings.EqualFold(c.XMLName.Local, "node") {
			return true
		}
	}
	return false
}

func convertXML(n rawXML, android bool) hNode {
	if !android {
		nodes := convertWDA(n, map[string]bool{})
		if len(nodes) == 1 {
			return nodes[0]
		}
		return hNode{Type: "Root", Children: nodes}
	}
	out := hNode{}
	out.Type = shortClass(n.attr("class"))
	out.ID = n.attr("resource-id")
	out.Text = n.attr("text")
	if out.Text == "" {
		out.Text = n.attr("content-desc")
	}
	out.Bounds = parseAndroidBounds(n.attr("bounds"))
	out.Enabled = enabledAttr(n)
	if n.attr("checkable") == "true" {
		out.Checked = boolPtr(n.attr("checked") == "true")
	}
	if n.attr("selected") == "true" {
		out.Selected = boolPtr(true)
	}
	if n.attr("focused") == "true" {
		out.Focused = boolPtr(true)
	}
	for _, c := range n.Children {
		out.Children = append(out.Children, convertXML(c, true))
	}
	return out
}

// convertWDA normalizes a WebDriverAgent element and its subtree.
//
// WebDriverAgent lists some elements twice: a React Native Text nested in a
// Text is a StaticText inside a StaticText with the same label and frame. The
// devicelab runner's simulator snapshot keeps the first element with a given
// type, label, identifier and origin and drops every later one, attaching its
// children to the kept element's parent. convertWDA applies that same rule
// in the same depth-first order, so a phone and a simulator list the text
// once. A dropped element returns its converted children in its place.
func convertWDA(n rawXML, seen map[string]bool) []hNode {
	out := hNode{}
	// The element tag name is the type; attrs carry name/label.
	out.Type = strings.TrimPrefix(n.XMLName.Local, "XCUIElementType")
	if t := n.attr("type"); t != "" {
		out.Type = strings.TrimPrefix(t, "XCUIElementType")
	}
	out.ID = n.attr("name")
	out.Text = n.attr("label")
	if out.Text == "" {
		out.Text = n.attr("value")
	}
	out.Bounds = parseWDABounds(n)
	out.Enabled = enabledAttr(n)
	if n.attr("selected") == "true" {
		out.Selected = boolPtr(true)
	}

	key := strings.Join([]string{out.Type, n.attr("label"), out.ID, n.attr("x"), n.attr("y")}, "\x00")
	duplicate := seen[key]
	seen[key] = true

	var children []hNode
	for _, c := range n.Children {
		children = append(children, convertWDA(c, seen)...)
	}
	if duplicate {
		return children
	}
	out.Children = children
	return []hNode{out}
}

// enabledAttr reads a node's enabled attribute, which UIAutomator and
// WebDriverAgent write on every element; nil when the node has none.
func enabledAttr(n rawXML) *bool {
	for _, a := range n.Attrs {
		if a.Name.Local == "enabled" {
			return boolPtr(a.Value != "false")
		}
	}
	return nil
}

func shortClass(class string) string {
	if class == "" {
		return ""
	}
	if i := strings.LastIndex(class, "."); i >= 0 {
		return class[i+1:]
	}
	return class
}

// parseAndroidBounds parses UIAutomator "[x1,y1][x2,y2]" into an hBounds.
func parseAndroidBounds(s string) *hBounds {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "][", ",")
	s = strings.Trim(s, "[]")
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return nil
	}
	n := make([]int, 4)
	for i, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil
		}
		n[i] = v
	}
	return &hBounds{X: n[0], Y: n[1], Width: n[2] - n[0], Height: n[3] - n[1]}
}

func parseWDABounds(n rawXML) *hBounds {
	x, y, w, h := n.attr("x"), n.attr("y"), n.attr("width"), n.attr("height")
	if x == "" && y == "" && w == "" && h == "" {
		return nil
	}
	atoi := func(s string) int { v, _ := strconv.Atoi(strings.TrimSpace(s)); return v }
	return &hBounds{X: atoi(x), Y: atoi(y), Width: atoi(w), Height: atoi(h)}
}

// ---- rendering ----

func renderCompact(n hNode, depth int, b *strings.Builder) {
	b.WriteString(compactLine(n, depth))
	b.WriteString("\n")
	for _, c := range n.Children {
		renderCompact(c, depth+1, b)
	}
}

func renderFind(n hNode, needle string, b *strings.Builder) {
	if nodeMatches(n, needle) {
		b.WriteString(compactLine(n, 0))
		b.WriteString("\n")
	}
	for _, c := range n.Children {
		renderFind(c, needle, b)
	}
}

func nodeMatches(n hNode, needle string) bool {
	return strings.Contains(strings.ToLower(n.Type), needle) ||
		strings.Contains(strings.ToLower(n.ID), needle) ||
		strings.Contains(strings.ToLower(n.Text), needle)
}

func compactLine(n hNode, depth int) string {
	var b strings.Builder
	b.WriteString(strings.Repeat("  ", depth))
	typ := n.Type
	if typ == "" {
		typ = "?"
	}
	b.WriteString(typ)
	if n.ID != "" {
		b.WriteString("  id=" + n.ID)
	}
	if n.Text != "" {
		b.WriteString(fmt.Sprintf("  text=%q", n.Text))
	}
	if n.Bounds != nil {
		b.WriteString(fmt.Sprintf("  (%d,%d %dx%d)", n.Bounds.X, n.Bounds.Y, n.Bounds.Width, n.Bounds.Height))
	}
	if n.Enabled != nil && !*n.Enabled {
		b.WriteString("  [disabled]")
	}
	if n.Checked != nil {
		if *n.Checked {
			b.WriteString("  [checked]")
		} else {
			b.WriteString("  [unchecked]")
		}
	}
	if n.Selected != nil && *n.Selected {
		b.WriteString("  [selected]")
	}
	if n.Focused != nil && *n.Focused {
		b.WriteString("  [focused]")
	}
	if n.Truncated != nil && *n.Truncated {
		b.WriteString("  [truncated]")
	}
	return b.String()
}
