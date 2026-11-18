package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/djeusette/cadre_cli/internal/api"
)

// runStatus is `cadre status [<project>]`: the queue and the active projects, or one project's milestones.
func runStatus(env Env, asJSON bool, args []string) int {
	if len(args) == 0 {
		return runCommand(env, asJSON, "status", args, 0, showStatus)
	}
	return runCommand(env, asJSON, "status", args, 1, func(ctx context.Context, s *session, a []string) int {
		return showProjectStatus(ctx, s, a[0])
	})
}

func showStatus(ctx context.Context, s *session, _ []string) int {
	if !s.requireKey() {
		return 1
	}
	shown, err := s.client.Status(ctx)
	if err != nil {
		return s.failAPI(err)
	}
	if s.asJSON {
		fmt.Fprint(s.env.Stdout, string(shown.Raw))
		return 0
	}
	fmt.Fprint(s.env.Stdout, statusText(shown))
	return 0
}

func showProjectStatus(ctx context.Context, s *session, slug string) int {
	if !s.requireKey() {
		return 1
	}
	shown, err := s.client.ProjectStatus(ctx, slug)
	if errors.Is(err, api.ErrProjectNotFound) {
		return s.projectNotFound(slug)
	}
	if err != nil {
		return s.failAPI(err)
	}
	if s.asJSON {
		fmt.Fprint(s.env.Stdout, string(shown.Raw))
		return 0
	}
	fmt.Fprint(s.env.Stdout, projectStatusText(shown))
	return 0
}

// statusText is the queue, then the active projects.
func statusText(status api.Status) string {
	var b strings.Builder
	b.WriteString(queueText(status.CanDeploy))
	if len(status.NothingToShip) > 0 {
		fmt.Fprintf(&b, "Nothing to ship: %s\n", strings.Join(status.NothingToShip, ", "))
	}
	if len(status.NotTracked) > 0 {
		fmt.Fprintf(&b, "Not tracked: %s\n", strings.Join(status.NotTracked, ", "))
	}
	fmt.Fprintf(&b, "\nProjects (%d active)\n", len(status.Projects))
	b.WriteString(projectsText(status.Projects))
	return b.String()
}

// queueText is a line per card, its name padded to the longest plus three spaces, and under `ships` a second line
// when the card unblocks something or has been deployed before.
func queueText(cards []api.QueueCard) string {
	if len(cards) == 0 {
		return "Can deploy now: nothing\n"
	}
	width := 0
	for _, c := range cards {
		width = max(width, utf8.RuneCountInString(c.Name))
	}
	width += 3

	var b strings.Builder
	b.WriteString("Can deploy now\n")
	for _, c := range cards {
		fmt.Fprintf(&b, "  %s%s\n", pad(c.Name, width), cardShips(c))
		if second := cardAfterwards(c); second != "" {
			fmt.Fprintf(&b, "  %s%s\n", strings.Repeat(" ", width), second)
		}
	}
	return b.String()
}

func cardShips(card api.QueueCard) string {
	ships := make([]string, len(card.Ships))
	for i, ship := range card.Ships {
		ships[i] = shipText(ship)
	}
	return "ships " + strings.Join(ships, ", ")
}

func shipText(ship api.Ship) string {
	words := []string{fmt.Sprintf("#%d", ship.Number)}
	if ship.Project == nil || ship.Milestone == nil {
		words = append(words, "not linked")
	} else {
		words = append(words, *ship.Project, *ship.Milestone)
	}
	if ship.BlockedBy != nil {
		words = append(words, "blocked by", pullRefText(*ship.BlockedBy))
	}
	if ship.Held {
		words = append(words, "held")
	}
	return strings.Join(words, " ")
}

func pullRefText(ref api.PullRef) string {
	if ref.Number == nil {
		return ref.Name
	}
	return fmt.Sprintf("%s #%d", ref.Name, *ref.Number)
}

// cardAfterwards is what a deploy of the card unblocks and when it last deployed, or "" when it has neither. The
// date is in UTC, the day the Overview shows, whatever this computer's zone.
func cardAfterwards(card api.QueueCard) string {
	var parts []string
	if len(card.Unblocks) > 0 {
		refs := make([]string, len(card.Unblocks))
		for i, ref := range card.Unblocks {
			refs[i] = pullRefText(ref)
		}
		parts = append(parts, "unblocks "+strings.Join(refs, ", "))
	}
	if card.LastDeployedAt != nil {
		parts = append(parts, "last deployed "+card.LastDeployedAt.UTC().Format("Jan 2"))
	}
	return strings.Join(parts, " · ")
}

// projectsText is a row per project: name, `1 of 4`, `2 · <title>`, the state's label and the next step, with `-`
// for what a project has none of.
func projectsText(projects []api.ProjectRow) string {
	rows := make([][]string, len(projects))
	for i, p := range projects {
		rows[i] = []string{p.Name, fmt.Sprintf("%d of %d", p.Deployed, p.Total), "-", "-", "-"}
		if p.Current != nil {
			rows[i][2] = fmt.Sprintf("%d · %s", p.Current.Number, p.Current.Title)
			rows[i][3] = p.Current.StateLabel
		}
		if p.NextStep != nil {
			rows[i][4] = p.NextStep.Text
		}
	}
	return table(rows)
}

// table prints rows under a two-space indent. The first column is padded to its longest value plus three spaces,
// every other column but the last to its longest plus two, and the last is not padded.
func table(rows [][]string) string {
	var widths []int
	for _, row := range rows {
		for i, cell := range row {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], utf8.RuneCountInString(cell))
		}
	}

	var b strings.Builder
	for _, row := range rows {
		b.WriteString("  ")
		for i, cell := range row {
			switch i {
			case len(row) - 1:
				b.WriteString(cell)
			case 0:
				b.WriteString(pad(cell, widths[i]+3))
			default:
				b.WriteString(pad(cell, widths[i]+2))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// pad appends spaces to text until it is width characters long. It counts characters, not bytes, since titles hold
// multi-byte ones such as the middle dot.
func pad(text string, width int) string {
	return text + strings.Repeat(" ", max(0, width-utf8.RuneCountInString(text)))
}

// projectStatusText is the project's header and next step, then each milestone with its steps.
func projectStatusText(project api.ProjectStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %d of %d deployed", project.Name, project.Deployed, project.Total)
	switch project.Group {
	case "finished":
		b.WriteString(" · Finished")
	case "archived":
		b.WriteString(" · Archived")
	}
	b.WriteString("\n")
	if project.NextStep != nil {
		fmt.Fprintf(&b, "Next: %s\n", nextStepText(*project.NextStep))
	}
	if len(project.Milestones) > 0 {
		b.WriteString("\n")
	}
	for _, m := range project.Milestones {
		fmt.Fprintf(&b, "%d · %s  %s\n", m.Number, m.Title, m.StateLabel)
		for _, step := range m.Steps {
			b.WriteString(stepText(step))
		}
		if m.NextStep != nil {
			fmt.Fprintf(&b, "    Next: %s\n", nextStepText(*m.NextStep))
		}
	}
	return b.String()
}

func nextStepText(next api.NextStep) string {
	if next.Meta == "" {
		return next.Text
	}
	return next.Text + " · " + next.Meta
}

// stepText is a line per pull request on the step, or one saying it has none.
func stepText(step api.StepStatus) string {
	name := step.Repo
	if step.App != nil {
		name += " / " + *step.App
	}
	if len(step.PullRequests) == 0 {
		return fmt.Sprintf("    %s  no PR yet\n", name)
	}
	var b strings.Builder
	for _, pr := range step.PullRequests {
		fmt.Fprintf(&b, "    %s  #%d  %s\n", name, pr.Number, pr.StatusLabel)
	}
	return b.String()
}
