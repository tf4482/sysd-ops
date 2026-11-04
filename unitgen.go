//go:build linux

package main

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"text/template"
	"text/template/parse"
)

// Unit kinds, also the template base names and file extensions
const (
	kindService = "service"
	kindTimer   = "timer"
)

// Source label of the embedded templates
const builtinSource = "built-in"

//go:embed templates/service.tmpl templates/timer.tmpl
var builtinTemplates embed.FS

// Template fields; the names are documented in the README and stay stable
type RenderData struct {
	Name               string
	Description        string
	ScriptPath         string
	Interpreter        string
	ExecStart          string
	WorkDir            string
	User               string
	Scope              string
	OnCalendar         string
	OnBootSec          string
	OnUnitActiveSec    string
	OnUnitInactiveSec  string
	Persistent         bool
	AccuracySec        string
	RandomizedDelaySec string
	Created            string
	Tool               string
}

// Parsed unit template with its origin and the fields it references
type unitTemplate struct {
	kind     string
	source   string
	template *template.Template
	fields   map[string]bool
}

// Templates by kind; directories are searched in order, then the built-in ones are used
func loadTemplates(directories []string) (map[string]unitTemplate, error) {
	templates := map[string]unitTemplate{}
	for _, kind := range []string{kindService, kindTimer} {
		file := kind + ".tmpl"
		source, data := builtinSource, []byte(nil)
		for _, directory := range directories {
			path := filepath.Join(directory, file)
			content, err := os.ReadFile(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("cannot read template %s: %v", path, err)
			}
			source, data = path, content
			break
		}
		if data == nil {
			data, _ = builtinTemplates.ReadFile("templates/" + file)
		}
		parsed, err := parseTemplate(kind, source, string(data))
		if err != nil {
			return nil, err
		}
		templates[kind] = parsed
	}
	return templates, nil
}

// Parse with missingkey=error and reject fields RenderData does not have
func parseTemplate(kind, source, text string) (unitTemplate, error) {
	name := kind + ".tmpl"
	if source != builtinSource {
		name = source
	}
	parsed, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return unitTemplate{}, fmt.Errorf("broken template %v", strings.TrimPrefix(err.Error(), "template: "))
	}
	known := reflect.TypeFor[RenderData]()
	fields := map[string]bool{}
	for _, associated := range parsed.Templates() {
		if associated.Tree == nil {
			continue
		}
		var unknown error
		walkFields(associated.Tree.Root, func(node *parse.FieldNode) {
			field := node.Ident[0]
			if _, ok := known.FieldByName(field); !ok && unknown == nil {
				location, _ := associated.Tree.ErrorContext(node)
				unknown = fmt.Errorf("broken template %s: unknown field .%s", location, field)
			}
			fields[field] = true
		})
		if unknown != nil {
			return unitTemplate{}, unknown
		}
	}
	return unitTemplate{kind, source, parsed, fields}, nil
}

// Visit every field reference of a parse tree
func walkFields(node parse.Node, visit func(*parse.FieldNode)) {
	switch node := node.(type) {
	case *parse.ListNode:
		if node != nil {
			for _, child := range node.Nodes {
				walkFields(child, visit)
			}
		}
	case *parse.ActionNode:
		walkFields(node.Pipe, visit)
	case *parse.IfNode:
		walkBranch(&node.BranchNode, visit)
	case *parse.RangeNode:
		walkBranch(&node.BranchNode, visit)
	case *parse.WithNode:
		walkBranch(&node.BranchNode, visit)
	case *parse.TemplateNode:
		walkFields(node.Pipe, visit)
	case *parse.PipeNode:
		if node != nil {
			for _, command := range node.Cmds {
				walkFields(command, visit)
			}
		}
	case *parse.CommandNode:
		for _, argument := range node.Args {
			walkFields(argument, visit)
		}
	case *parse.ChainNode:
		walkFields(node.Node, visit)
	case *parse.FieldNode:
		visit(node)
	}
}

// Condition and both branches of if, range, and with
func walkBranch(node *parse.BranchNode, visit func(*parse.FieldNode)) {
	walkFields(node.Pipe, visit)
	walkFields(node.List, visit)
	walkFields(node.ElseList, visit)
}

// Rendered unit file, checked for content and the expected sections
func (u unitTemplate) render(data RenderData) (string, error) {
	var output strings.Builder
	if err := u.template.Execute(&output, data); err != nil {
		return "", fmt.Errorf("broken template %v", strings.TrimPrefix(err.Error(), "template: "))
	}
	text := output.String()
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("template %s rendered an empty unit", u.source)
	}
	lines := strings.Split(text, "\n")
	for _, section := range []string{"[Unit]", "[" + strings.ToUpper(u.kind[:1]) + u.kind[1:] + "]"} {
		found := false
		for _, line := range lines {
			found = found || strings.TrimSpace(line) == section
		}
		if !found {
			return "", fmt.Errorf("template %s rendered no %s section", u.source, section)
		}
	}
	return text, nil
}

// One ExecStart= word per systemd rules: specifiers, variables, and quoting
func execWord(word string) string {
	word = strings.NewReplacer("%", "%%", "$", "$$").Replace(word)
	if word != "" && word != ";" && !strings.ContainsAny(word, " \t\"'\\") {
		return word
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(word) + `"`
}

// Value of a plain setting such as Description=: specifiers escaped
func settingValue(value string) string {
	return strings.ReplaceAll(value, "%", "%%")
}

// First word of an ExecStart= value without prefixes, quotes, and escapes
func execProgram(value string) string {
	value = strings.TrimLeft(strings.TrimSpace(value), "-@:+!|")
	var word strings.Builder
	quoted := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character == '"':
			quoted = !quoted
		case character == '\\' && index+1 < len(value):
			index++
			word.WriteByte(value[index])
		case (character == ' ' || character == '\t') && !quoted:
			return unescapeWord(word.String())
		default:
			word.WriteByte(character)
		}
	}
	return unescapeWord(word.String())
}

// Undo specifier and variable escaping
func unescapeWord(word string) string {
	return strings.NewReplacer("%%", "%", "$$", "$").Replace(word)
}

// ExecStart= value of a rendered service file, empty when missing
func execStartLine(unit string) string {
	value := ""
	for line := range strings.SplitSeq(unit, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart="); ok {
			value = rest
		}
	}
	return value
}
