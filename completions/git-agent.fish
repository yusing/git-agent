# fish completion for git-agent

function __git_agent_no_subcommand
    set -l words (commandline -opc)
    test (count $words) -le 1
end

function __git_agent_has_subcommand
    set -l words (commandline -opc)
    test (count $words) -gt 1; and contains -- $words[2] commit-msg pr-message release-note
end

function __git_agent_using_command
    set -l words (commandline -opc)
    test (count $words) -gt 1; and test $words[2] = $argv[1]
end

function __git_agent_git_refs
    command git for-each-ref --format='%(refname:short)' refs/heads refs/remotes refs/tags 2>/dev/null
    command git rev-parse --short HEAD 2>/dev/null
end

complete -c git-agent -f

complete -c git-agent -n '__git_agent_no_subcommand' -a commit-msg -d 'Generate a commit message from staged changes'
complete -c git-agent -n '__git_agent_no_subcommand' -a pr-message -d 'Generate a pull request message from branch changes'
complete -c git-agent -n '__git_agent_no_subcommand' -a release-note -d 'Generate a release note for a ref range'
complete -c git-agent -n '__git_agent_no_subcommand' -a help -d 'Show usage'

complete -c git-agent -n '__git_agent_using_command commit-msg' -l amend -d 'Generate an amended commit message'

complete -c git-agent -n '__git_agent_has_subcommand' -l model -r -d 'Override OPENAI_MODEL'
complete -c git-agent -n '__git_agent_has_subcommand' -l fast -d 'Use priority service tier'
complete -c git-agent -n '__git_agent_has_subcommand' -l low -d 'Use low reasoning effort'
complete -c git-agent -n '__git_agent_has_subcommand' -l medium -d 'Use medium reasoning effort'
complete -c git-agent -n '__git_agent_has_subcommand' -l high -d 'Use high reasoning effort'
complete -c git-agent -n '__git_agent_has_subcommand' -l xhigh -d 'Use xhigh reasoning effort'
complete -c git-agent -n '__git_agent_has_subcommand' -l base-url -r -d 'Override provider base URL'
complete -c git-agent -n '__git_agent_has_subcommand' -l timeout -r -d 'Override default request timeout'
complete -c git-agent -n '__git_agent_has_subcommand' -l max-steps -r -d 'Override maximum agent steps'
complete -c git-agent -n '__git_agent_has_subcommand' -l guidance-family -r -a 'auto agents claude codex none' -d 'Force guidance family'
complete -c git-agent -n '__git_agent_has_subcommand' -l debug -d 'Enable debug output on stderr'

complete -c git-agent -n '__git_agent_using_command release-note' -a '(__git_agent_git_refs)' -d 'Git ref'
