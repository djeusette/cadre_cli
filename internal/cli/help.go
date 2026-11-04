package cli

import (
	"fmt"
	"slices"
	"strings"
)

// commandHelp documents one command. helpSections holds them all, and every usage line and help page comes from there.
type commandHelp struct {
	name     string   // the words that call it, such as "spec push"
	summary  string   // one line for the lists, which also opens the command's page
	forms    []string // what can follow the name, one entry per way to call it; none when nothing can
	about    string   // what the summary leaves out, hard-wrapped to 80 columns
	flags    []helpRow
	examples []string
}

// helpRow is one line of a two-column list.
type helpRow struct{ left, right string }

// helpSection is a titled part of the overview.
type helpSection struct {
	title    string
	commands []commandHelp
}

// groupHelp documents a word that only leads to commands, such as "project".
type groupHelp struct{ name, summary string }

// helpRequest removes --help, -h and a leading help from args, and reports whether any of them was there.
func helpRequest(args []string) ([]string, bool) {
	args, long := splitBoolFlag(args, "--help")
	args, short := splitBoolFlag(args, "-h")
	if long || short {
		return args, true
	}
	if len(args) > 0 && args[0] == "help" {
		return args[1:], true
	}
	return args, false
}

// showHelp prints the page of the command or group that the longest leading words of args name, or the overview when
// args is empty. Whatever follows the name, such as the command's own arguments, is ignored.
func showHelp(env Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stdout, overviewPage())
		return 0
	}
	for n := len(args); n > 0; n-- {
		if page, ok := helpPage(strings.Join(args[:n], " ")); ok {
			fmt.Fprint(env.Stdout, page)
			return 0
		}
	}
	return unknownCommand(env, "", args[0])
}

func helpPage(name string) (string, bool) {
	if c, ok := findCommand(name); ok {
		return c.page(), true
	}
	for _, g := range helpGroups {
		if g.name == name {
			return g.page(), true
		}
	}
	return "", false
}

func findCommand(name string) (commandHelp, bool) {
	commands := allCommands()
	i := slices.IndexFunc(commands, func(c commandHelp) bool { return c.name == name })
	if i < 0 {
		return commandHelp{}, false
	}
	return commands[i], true
}

func allCommands() []commandHelp {
	var commands []commandHelp
	for _, section := range helpSections {
		commands = append(commands, section.commands...)
	}
	return commands
}

// usageError prints how to call the named command and returns the exit code of a wrong call.
func usageError(env Env, name string) int {
	c, _ := findCommand(name)
	prefix := "Usage: "
	for _, line := range c.usage() {
		fmt.Fprintln(env.Stderr, prefix+line)
		prefix = "       "
	}
	fmt.Fprintf(env.Stderr, "Run cadre help %s for details.\n", name)
	return 2
}

// unknownCommand says that word is not a command of group, which is empty for cadre itself, and returns the exit code
// of a wrong call.
func unknownCommand(env Env, group, word string) int {
	parent := strings.TrimSpace("cadre " + group)
	fmt.Fprintf(env.Stderr, "%s has no %s command.\n", parent, word)
	if near := closest(word, nextWords(group)); near != "" {
		fmt.Fprintf(env.Stderr, "Did you mean %s %s?\n", parent, near)
	}
	fmt.Fprintf(env.Stderr, "Run %s to see its commands.\n", strings.TrimSpace("cadre help "+group))
	return 2
}

// nextWords lists the words that can follow group on the command line, each once.
func nextWords(group string) []string {
	var words []string
	for _, c := range commandsIn(group) {
		word, _, _ := strings.Cut(c.nameIn(group), " ")
		if !slices.Contains(words, word) {
			words = append(words, word)
		}
	}
	return words
}

// commandsIn lists the commands under group, which is empty for all of them.
func commandsIn(group string) []commandHelp {
	if group == "" {
		return allCommands()
	}
	return slices.DeleteFunc(allCommands(), func(c commandHelp) bool {
		return !strings.HasPrefix(c.name, group+" ")
	})
}

// nameIn is the command's name without the words of group.
func (c commandHelp) nameIn(group string) string {
	return strings.TrimSpace(strings.TrimPrefix(c.name, group))
}

// closest is the word nearest to typed, or "" when none is near enough to be a typo of it.
func closest(typed string, words []string) string {
	best, bestDistance := "", max(1, len(typed)/3)+1
	for _, word := range words {
		if d := editDistance(typed, word); d < bestDistance {
			best, bestDistance = word, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b, counted in bytes: command names are ASCII.
func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			substitution := previous[j-1]
			if a[i-1] != b[j-1] {
				substitution++
			}
			current[j] = min(previous[j]+1, current[j-1]+1, substitution)
		}
		previous = current
	}
	return previous[len(b)]
}

// usage lists the ways to call the command, one line each.
func (c commandHelp) usage() []string {
	if len(c.forms) == 0 {
		return []string{"cadre " + c.name}
	}
	lines := make([]string, len(c.forms))
	for i, form := range c.forms {
		lines[i] = "cadre " + c.name + " " + form
	}
	return lines
}

func (c commandHelp) page() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s.\n\nUsage:\n", c.summary)
	writeLines(&b, c.usage())
	if c.about != "" {
		fmt.Fprintf(&b, "\n%s\n", c.about)
	}
	if len(c.flags) > 0 {
		b.WriteString("\nFlags:\n")
		writeRows(&b, c.flags, rowWidth(c.flags))
	}
	if len(c.examples) > 0 {
		b.WriteString("\nExamples:\n")
		writeLines(&b, c.examples)
	}
	return b.String()
}

func (g groupHelp) page() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s.\n\nUsage:\n", g.summary)
	var rows []helpRow
	for _, c := range commandsIn(g.name) {
		writeLines(&b, c.usage())
		rows = append(rows, helpRow{c.nameIn(g.name), c.summary})
	}
	b.WriteString("\nCommands:\n")
	writeRows(&b, rows, rowWidth(rows))
	fmt.Fprintf(&b, "\nRun cadre help %s <command> for its flags and examples.\n", g.name)
	return b.String()
}

func overviewPage() string {
	var rows []helpRow
	for _, c := range allCommands() {
		rows = append(rows, helpRow{c.name, c.summary})
	}
	width := rowWidth(rows)

	var b strings.Builder
	b.WriteString("cadre shows where every milestone stands, on every repo, in deploy order.\n\n")
	b.WriteString("Usage:\n  cadre <command> [arguments]\n")
	for _, section := range helpSections {
		fmt.Fprintf(&b, "\n%s:\n", section.title)
		for _, c := range section.commands {
			writeRows(&b, []helpRow{{c.name, c.summary}}, width)
		}
	}
	b.WriteString("\nFlags:\n")
	writeRows(&b, globalFlags, rowWidth(globalFlags))
	b.WriteString("\nRun cadre help <command> for its arguments, flags and examples.\n")
	return b.String()
}

func writeLines(b *strings.Builder, lines []string) {
	for _, line := range lines {
		fmt.Fprintf(b, "  %s\n", line)
	}
}

func writeRows(b *strings.Builder, rows []helpRow, width int) {
	for _, row := range rows {
		fmt.Fprintf(b, "  %-*s  %s\n", width, row.left, row.right)
	}
}

func rowWidth(rows []helpRow) int {
	width := 0
	for _, row := range rows {
		width = max(width, len(row.left))
	}
	return width
}
