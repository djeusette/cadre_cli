package cli

import "context"

// runVercel dispatches `vercel team add <team>` and `vercel team remove <team>`.
func runVercel(env Env, asJSON bool, args []string) int {
	switch {
	case len(args) == 0:
		return showHelp(env, []string{"vercel"})
	case args[0] != "team":
		return unknownCommand(env, "vercel", args[0])
	case len(args) == 1:
		return showHelp(env, []string{"vercel", "team"})
	}
	rest := args[2:]
	switch args[1] {
	case "add":
		return runCommand(env, asJSON, "vercel team add", rest, 1, func(ctx context.Context, s *session, a []string) int {
			return addAccount(ctx, s, "vercel", "team", a[0])
		})
	case "remove":
		return runCommand(env, asJSON, "vercel team remove", rest, 1, func(ctx context.Context, s *session, a []string) int {
			return removeAccount(ctx, s, "vercel", "team", a[0])
		})
	default:
		return unknownCommand(env, "vercel team", args[1])
	}
}
