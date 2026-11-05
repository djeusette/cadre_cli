package cli

var jsonFlag = helpRow{"--json", "Print the server's answer as JSON"}

var globalFlags = []helpRow{
	{"--json", "Print the answer as JSON, for scripts and Claude Code"},
	{"-h, --help", "Show help for any command"},
}

var helpGroups = []groupHelp{
	{"project", "Register, change and archive projects"},
	{"spec", "Read and write milestone specs"},
	{"pr", "Link pull requests to milestones"},
	{"repo", "Say where repos deploy"},
	{"fly", "Connect Fly orgs, so cadre can read their apps' deploys"},
	{"fly org", "Connect Fly orgs, so cadre can read their apps' deploys"},
}

// helpSections is every command, in the overview's order.
var helpSections = []helpSection{
	{
		title: "Start here",
		commands: []commandHelp{
			{
				name:    "login",
				summary: "Sign in to a Cadre server from your browser",
				forms:   []string{"--url <url>"},
				about: `Opens the server's approval page. Once you approve, cadre stores the URL and a
personal API key in ~/.config/cadre/config.json. Every other command talks to
that URL, and only that one.`,
				flags: []helpRow{
					{"--url <url>", "The Cadre server to sign in to. Required"},
					{"--json", "Print the email and URL as JSON"},
				},
				examples: []string{"cadre login --url https://cadre.example.com"},
			},
			{
				name:    "whoami",
				summary: "Show the server and account you're signed in as",
				flags:   []helpRow{{"--json", "Print the email and URL as JSON"}},
			},
		},
	},
	{
		title: "Projects",
		commands: []commandHelp{
			{
				name:    "project create",
				summary: "Register a project and its milestones from a JSON file",
				forms:   []string{"<file>"},
				about: `Each milestone's repos are its deploy order, and its spec is markdown. Only
name, slug, end_goal and each milestone's title and repos are required:

  {
    "name": "Wish reminders",
    "slug": "wish-reminders",
    "end_goal": "Members never miss a wish that is about to expire.",
    "milestones": [
      {
        "title": "Members get a reminder before a wish expires",
        "repos": ["shop-api", "shop-mobile"],
        "spec": "# Members get a reminder\n\n...",
        "design_url": "https://claude.ai/design/p/..."
      }
    ],
    "canvas_url": "https://claude.ai/design/p/..."
  }`,
				flags:    []helpRow{jsonFlag},
				examples: []string{"cadre project create project.json"},
			},
			{
				name:    "project pull",
				summary: "Print a project as the JSON file project update takes",
				forms:   []string{"<slug> [-o <file>]"},
				about:   "The file has the version and milestone numbers that update needs.",
				flags: []helpRow{
					{"-o <file>", "Write the file to <file> instead of printing it"},
					jsonFlag,
				},
				examples: []string{"cadre project pull wish-reminders -o project.json"},
			},
			{
				name:    "project update",
				summary: "Apply a pulled and edited project file",
				forms:   []string{"<file>"},
				about: `Sends the file, unchanged, to the project named by its slug, and prints the new
version and the project's page. A milestone left out of the file is archived.
Refused when someone updated the project since you pulled it, or when it is
archived.`,
				flags: []helpRow{jsonFlag},
				examples: []string{
					"cadre project pull wish-reminders -o project.json",
					"cadre project update project.json",
				},
			},
			{
				name:     "project archive",
				summary:  "Hide a project from the Overview",
				forms:    []string{"<slug>"},
				about:    "Its page still opens. cadre project unarchive brings it back.",
				flags:    []helpRow{jsonFlag},
				examples: []string{"cadre project archive wish-reminders"},
			},
			{
				name:     "project unarchive",
				summary:  "Bring an archived project back",
				forms:    []string{"<slug>"},
				flags:    []helpRow{jsonFlag},
				examples: []string{"cadre project unarchive wish-reminders"},
			},
		},
	},
	{
		title: "Milestone specs",
		commands: []commandHelp{
			{
				name:    "spec pull",
				summary: "Print the newest version of a milestone's spec",
				forms:   []string{"<project>/<n> [-o <file>]"},
				about: `<project>/<n> is a project's slug and a milestone number, such as
wish-reminders/2. The spec comes exactly as stored.`,
				flags: []helpRow{
					{"-o <file>", "Write the spec to <file> instead of printing it"},
					{"--json", "Print the answer as JSON, with the version --base takes"},
				},
				examples: []string{
					"cadre spec pull wish-reminders/2",
					"cadre spec pull wish-reminders/2 -o spec.md --json",
				},
			},
			{
				name:    "spec push",
				summary: "Upload a markdown file as a milestone's newest spec",
				forms:   []string{"<project>/<n> <file> [--base <version>]"},
				about: `<project>/<n> is a project's slug and a milestone number, such as
wish-reminders/2. The file is stored unchanged, and cadre prints the project's
page.`,
				flags: []helpRow{
					{"--base <version>", "Refuse the push if someone pushed since this version"},
					jsonFlag,
				},
				examples: []string{
					`cadre spec pull wish-reminders/2 -o spec.md --json   # note the "version"`,
					"cadre spec push wish-reminders/2 spec.md --base <version>",
				},
			},
		},
	},
	{
		title: "Pull requests",
		commands: []commandHelp{
			{
				name:    "pr link",
				summary: "Link a pull request to a milestone, or unlink it",
				forms:   []string{"<pr-url> <project>/<n>", "<pr-url> --none"},
				about: `<pr-url> is the pull request's GitHub URL. <project>/<n> is a project's slug and
a milestone number, such as wish-reminders/2.`,
				flags: []helpRow{
					{"--none", "Unlink the pull request from its milestone"},
					jsonFlag,
				},
				examples: []string{
					"cadre pr link https://github.com/acme/shop-api/pull/418 wish-reminders/2",
					"cadre pr link https://github.com/acme/shop-api/pull/418 --none",
				},
			},
		},
	},
	{
		title: "Deploy targets",
		commands: []commandHelp{
			{
				name:    "repo set",
				summary: "Say where a repo deploys",
				forms:   []string{"<repo> --fly-app <app> [--fly-config <file>]", "<repo> --platform <name>"},
				about: `<repo> is the repo's name in Cadre, such as shop-api. Setting a platform again
replaces its settings, so pass both --fly-app and --fly-config each time. Fly
apps are found through the orgs connected with cadre fly org add.`,
				flags: []helpRow{
					{"--fly-app <app>", "Connect the repo to this Fly app, which cadre then tracks"},
					{"--fly-config <file>", "The Fly config file it deploys with, for the whole team"},
					{"--platform <name>", "Name any other platform, which shows as not tracked"},
					jsonFlag,
				},
				examples: []string{
					"cadre repo set shop-api --fly-app shop-api-prod",
					"cadre repo set shop-api --fly-app shop-api-prod --fly-config fly.staging.toml",
					"cadre repo set shop-site --platform render",
				},
			},
			{
				name:    "repo unset",
				summary: "Remove a repo's deploy target",
				forms:   []string{"<repo> --platform <name>"},
				flags: []helpRow{
					{"--platform <name>", "The platform to remove the target from. Required"},
					jsonFlag,
				},
				examples: []string{"cadre repo unset shop-site --platform render"},
			},
			{
				name:    "repo show",
				summary: "List a repo's deploy targets and last deploys",
				forms:   []string{"<repo>"},
				about: `Prints each target's platform, the Fly org it is read through, whether cadre can
track it, and its last deploy: the date in UTC and the commit.`,
				flags:    []helpRow{jsonFlag},
				examples: []string{"cadre repo show shop-api"},
			},
			{
				name:    "fly org add",
				summary: "Connect a Fly org with a read-only token",
				forms:   []string{"<org>"},
				about: `Pipe the token in, or type it when asked, with echo off. It is never an
argument, so it stays out of your shell history. Adding an org again replaces
its token.`,
				flags:    []helpRow{jsonFlag},
				examples: []string{"fly tokens create readonly -o acme -n cadre | cadre fly org add acme"},
			},
			{
				name:     "fly org remove",
				summary:  "Disconnect a Fly org",
				forms:    []string{"<org>"},
				about:    "Refused while a repo's target still reads the org.",
				flags:    []helpRow{jsonFlag},
				examples: []string{"cadre fly org remove acme"},
			},
			{
				name:    "deploy",
				summary: "Deploy a repo to its Fly app, from a checkout of it",
				forms:   []string{"<repo> [--yes]"},
				about: `<repo> is the repo's name in Cadre, such as shop-api. Run it anywhere in a
checkout of that repo. cadre runs fly deploy at the checkout's top level, with
the commit as the image label, so Cadre can tie the deploy to it.

It refuses unless the checkout is on main, has no uncommitted changes or
untracked files, and main equals origin/main after a git fetch. The repo needs a
Fly app, set with cadre repo set, and fly must be installed and signed in.

The config file is the one set with --fly-config, else fly.production.toml if
it's committed, else fly.toml. It refuses unless that file is committed and on
disk, so an ignored local file never decides what deploys.

Open Before deploying items are listed, and cadre asks before going on. With
nothing to read, the answer is no. If Cadre can't say what is open, nothing
deploys. Right before fly runs, cadre checks the branch, the commit and the
working tree again, so a change made in the meantime stops the deploy.`,
				flags: []helpRow{
					{"--yes", "Go on despite open Before deploying items"},
					{"--json", "Print what was deployed as JSON, with fly's output on stderr"},
				},
				examples: []string{
					"cadre deploy shop-api",
					"cadre deploy shop-api --yes",
				},
			},
		},
	},
	{
		title: "This CLI",
		commands: []commandHelp{
			{
				name:    "upgrade",
				summary: "Replace this cadre with the latest release",
				about: `Installs the latest GitHub release once the download matches its checksums, and
prints both versions. Changes nothing when this is already the latest, and
refuses a development build. Needs no sign-in.`,
				flags: []helpRow{{"--json", "Print the versions and the binary's path as JSON"}},
			},
			{
				name:    "version",
				summary: "Print the version",
			},
			{
				name:    "help",
				summary: "Show help for a command",
				forms:   []string{"[<command>]"},
				about:   "With no command, lists them all. cadre <command> --help does the same.",
				examples: []string{
					"cadre help",
					"cadre help project",
					"cadre help spec push",
				},
			},
		},
	},
}
