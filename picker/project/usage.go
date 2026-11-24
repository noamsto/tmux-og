package project

// Usage is og-project-bin's usage text; og prints it for `og project --help` because og intercepts a sole --help.
const Usage = `Usage: og project <subcommand>

Subcommands:
  status [<project>]   Per host: reachable, checkout path, worktree count and
                       default-branch drift, for every project or one.

<project> is a project key (github.com/<owner>/<repo>), a name declared in
programs.tmux-og.projects, or any origin URL of the project.
`
