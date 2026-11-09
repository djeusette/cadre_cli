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
	{"vercel", "Connect Vercel teams, so cadre can read their apps' deploys"},
	{"vercel team", "Connect Vercel teams, so cadre can read their apps' deploys"},
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
				forms:   []string{"<pr-url> <project>/<n> [--app <name>]", "<pr-url> --none"},
				about: `<pr-url> is the pull request's GitHub URL. <project>/<n> is a project's slug and
a milestone number, such as wish-reminders/2. The Cadre: <project>/<n> <app>
line in a pull request's description does the same as --app.
That line wins at the pull request's next sync from GitHub, so an app set by
hand changes with it.`,
				flags: []helpRow{
					{"--app <name>", "Link it to that app's step, by its app name in Cadre"},
					{"--none", "Unlink the pull request from its milestone"},
					jsonFlag,
				},
				examples: []string{
					"cadre pr link https://github.com/acme/shop-api/pull/418 wish-reminders/2",
					"cadre pr link https://github.com/acme/shop-api/pull/37 live-scores/2 --app api",
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
				forms: []string{
					"<repo> --fly-app <app> [--name <app>] [--fly-config <file>]",
					"<repo> --vercel-project <id> --name <app> --vercel-dir <folder>",
					"<repo> --platform <name>",
				},
				about: `<repo> is the repo's name in Cadre, such as shop-api. Setting an app again (the
same name) replaces its settings, so pass --fly-app, --fly-config and
--status-url each time, and --name when the app's name isn't its Fly app's. Fly
apps are found through the orgs connected with cadre fly org add.

A repo can have several Fly apps, each with a name that is unique in the repo.
--fly-app names the Fly app, and the app takes its name unless --name gives
another. Set a name again to change that app; --fly-app with another name adds
an app, and says so.

A repo can have several Vercel apps, one per --name. Give each its project id
and the folder it builds from, and set it again to change it. Vercel projects
are found through the teams connected with cadre vercel team add.`,
				flags: []helpRow{
					{"--fly-app <app>", "Connect the repo to a Fly app, which cadre then tracks"},
					{"--fly-config <file>", "The Fly config file it deploys with, for the whole team"},
					{"--status-url <url>", "An https URL cadre checks to tell if the app is live"},
					{"--vercel-project <id>", "Connect the repo to a Vercel project cadre tracks"},
					{"--name <app>", "The app's name: required on Vercel, else the Fly app's"},
					{"--vercel-dir <folder>", "The folder the app deploys from. Vercel only"},
					{"--platform <name>", "Name any other platform, which shows as not tracked"},
					jsonFlag,
				},
				examples: []string{
					"cadre repo set shop-api --fly-app shop-api-prod",
					"cadre repo set shop-api --fly-app shop-api-prod --fly-config fly.staging.toml",
					"cadre repo set shop-api --fly-app shop-api-worker --name worker",
					"cadre repo set shop-api --fly-app shop-api --status-url https://app.test/up",
					"cadre repo set shop-api --vercel-project prj_1 --name site --vercel-dir web",
					"cadre repo set shop-site --platform render",
				},
			},
			{
				name:    "repo unset",
				summary: "Remove a repo's deploy target",
				forms: []string{
					"<repo> --name <app>",
					"<repo> --platform <name> [--name <app>]",
				},
				about: `--name alone removes that app, whatever its platform. --platform alone removes
the repo's target there, when the repo has one app on it.`,
				flags: []helpRow{
					{"--name <app>", "The app to remove. Required unless --platform is given"},
					{"--platform <name>", "The platform to remove the target from"},
					jsonFlag,
				},
				examples: []string{
					"cadre repo unset shop-site --platform render",
					"cadre repo unset shop-api --platform vercel --name site",
					"cadre repo unset shop-api --name worker",
				},
			},
			{
				name:    "repo show",
				summary: "List a repo's deploy targets and last deploys",
				forms:   []string{"<repo>"},
				about: `Prints each target's platform, the Fly org or Vercel team it is read through,
whether cadre can track it, and its last deploy: the date in UTC and the commit.
A Vercel target starts with its name, and so does a Fly app whose name isn't its
Fly app's. A line under the target gives its status URL, if it has one, and
whether the app is live: live, not live since when, or not checked yet.`,
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
				name:    "vercel team add",
				summary: "Connect a Vercel team with a token that reads it",
				forms:   []string{"<team>"},
				about: `A Vercel token is made in Vercel's dashboard, under Account Settings, Tokens,
scoped to the team: the Vercel CLI can't make one. Pipe it in, or type it when
asked, with echo off. It is never an argument, so it stays out of your shell
history. Adding a team again replaces its token.`,
				flags:    []helpRow{jsonFlag},
				examples: []string{"cadre vercel team add colette-club"},
			},
			{
				name:     "vercel team remove",
				summary:  "Disconnect a Vercel team",
				forms:    []string{"<team>"},
				about:    "Refused while a repo's target still reads the team.",
				flags:    []helpRow{jsonFlag},
				examples: []string{"cadre vercel team remove colette-club"},
			},
			{
				name:    "deploy",
				summary: "Deploy a repo's Fly or Vercel apps, from a checkout of it",
				forms:   []string{"<repo> [--app <name>|--all] [--yes]"},
				about: `<repo> is the repo's name in Cadre, such as shop-api. Run it anywhere in a
checkout of that repo. A repo with one app needs no flag. A repo with several
needs --app <name> for one of them, or --all for every one. --all refuses a
repo with apps on more than one platform.

For a Fly app, cadre runs fly deploy at the checkout's top level, with the
commit as the image label, so Cadre can tie the deploy to it. It refuses unless
the checkout is on main, has no uncommitted changes or untracked files, and main
equals origin/main after a git fetch. The repo needs a Fly app, set with cadre
repo set, and fly must be installed and signed in. With --all, the Fly apps
deploy one at a time, in the order Cadre's release queue lists them, and the
first that fails stops the ones after it. A signal such as Ctrl-C also stops
them, before the next app. A run that stops part-way names each app after the
one it stopped at.

The config file is the one set with --fly-config, else fly.production.toml if
it's committed, else fly.toml. That file must be committed and on disk, or it
refuses. Every app's file is checked before the first app deploys.

For Vercel apps, cadre deploys the commit the checkout is on, from any branch,
once origin has it after a git fetch. The tree must be clean, and each app's
folder must be in the checkout and hold a .vercelignore, as vercel uploads the
folder and doesn't read .gitignore. Every app builds at once, each in its
folder, with vercel deploy --prod --skip-domain. Once every build is ready,
cadre runs vercel promote for each, so they go live together. It says which
commit it builds for which apps, then that it promotes them. A failed build
promotes nothing and shows what vercel printed, and a failed promotion stops
the ones after it and says which apps went live. --app builds and promotes that
app alone. vercel must be installed and signed in.

Open Before deploying items are listed, and so are the pull requests it would
ship that Cadre's release queue shows Blocked, each with what should deploy
first. With --app, only that app's are listed. cadre asks once, before the
first app deploys. It asks the same, in one question, when the commit lacks
what a Vercel app runs now. With nothing to read, the answer is no. If Cadre
can't say what is open or Blocked, nothing deploys. Right before fly or vercel
runs, cadre checks the commit and the working tree again, and for Fly the
branch, so a change made in the meantime stops the deploy.`,
				flags: []helpRow{
					{"--app <name>", "Deploy that app alone, by its name in Cadre"},
					{"--all", "Deploy every app of the repo"},
					{"--yes", "Go on without asking about what is listed before the deploy"},
					{"--json", "Print one JSON object per app deployed, and all else on stderr"},
				},
				examples: []string{
					"cadre deploy shop-api",
					"cadre deploy shop-api --yes",
					"cadre deploy shop-api --app shop-worker",
					"cadre deploy shop-web --all",
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
