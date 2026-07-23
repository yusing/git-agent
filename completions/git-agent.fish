# fish completion for git-agent

complete -c git-agent -e

function __git_agent_no_subcommand
    set -l words (commandline -opc)
    test (count $words) -le 1
end

function __git_agent_config_needs_key
    set -l words (commandline -opc)
    test (count $words) -gt 1; and test "$words[2]" = config; or return 1

    set -l args $words[3..-1]
    test (count $args) -eq 0; and return 0
    test (count $args) -eq 1; and test "$args[1]" = --unset
end

function __git_agent_config_can_unset
    set -l words (commandline -opc)
    test (count $words) -eq 2; and test "$words[2]" = config
end

function __git_agent_needs_index_subcommand
    set -l words (commandline -opc)
    test (count $words) -eq 2; and test "$words[2]" = index
end

function __git_agent_index_migrate_can_complete
    set -l candidate $argv[1]
    set -l words (commandline -opc)
    test (count $words) -gt 2; and test "$words[2]" = index; and test "$words[3]" = migrate; or return 1

    set -l seen_to 0
    set -l seen_dry_run 0
    set -l needs_to_value 0

    for word in $words[4..-1]
        if test $needs_to_value -eq 1
            test "$word" = v2; or return 1
            set needs_to_value 0
            continue
        end

        switch $word
            case --to
                test $seen_to -eq 0; or return 1
                set seen_to 1
                set needs_to_value 1
            case --dry-run
                test $seen_dry_run -eq 0; or return 1
                set seen_dry_run 1
            case '*'
                return 1
        end
    end

    switch $candidate
        case to
            test $seen_to -eq 0; or test $needs_to_value -eq 1
        case dry-run
            test $needs_to_value -eq 0; and test $seen_dry_run -eq 0
        case option-to
            test $seen_to -eq 0
        case option-dry-run
            test $needs_to_value -eq 0; and test $seen_dry_run -eq 0
        case '*'
            return 1
    end
end

function __git_agent_command_has_option
    set -l command_name $argv[1]
    set -l option $argv[2]
    set -l shared model fast low medium high xhigh base-url timeout max-steps guidance-family append-prompt debug pprof

    switch $command_name
        case commit commit-msg
            contains -- "$option" amend $shared
        case pr-message
            contains -- "$option" $shared
        case release-note
            contains -- "$option" out $shared
        case review simplify
            contains -- "$option" codebase uncommitted staged wait follow-up depth max-web-searches dry-run help orchestration-artifact $shared
        case search
            contains -- "$option" rev remote scope min-score limit format index reindex code no-tests agent ls ls-remotes ls-files embedding-model embedding-dimensions base-url timeout debug pprof
        case '*'
            return 1
    end
end

function __git_agent_option_takes_value
    contains -- "$argv[1]" model base-url timeout max-steps guidance-family append-prompt pprof wait follow-up depth max-web-searches orchestration-artifact out rev remote scope min-score limit format embedding-model embedding-dimensions
end

function __git_agent_option_value_is_valid
    set -l command_name $argv[1]
    set -l option $argv[2]
    set -l value $argv[3]

    switch $option
        case depth
            contains -- "$command_name" review simplify; and contains -- "$value" fast balanced thorough
        case guidance-family
            contains -- "$value" auto agents claude codex none
        case format
            test "$command_name" = search; and contains -- "$value" json brief text tree completion
        case embedding-model
            return 0
        case embedding-dimensions
            string match -qr '^\+?0*[1-9][0-9]*$' -- "$value"
        case '*'
            return 0
    end
end

function __git_agent_bool_value_is_valid
    contains -- "$argv[1]" 1 t T TRUE true True 0 f F FALSE false False
end

function __git_agent_bool_value_is_true
    contains -- "$argv[1]" 1 t T TRUE true True
end

function __git_agent_search_mode_allows_option
    set -l mode $argv[1]
    set -l option $argv[2]
    switch $mode
        case ls
            contains -- "$option" ls remote format
        case ls-remotes
            contains -- "$option" ls-remotes format
        case ls-files
            contains -- "$option" ls-files format remote rev scope no-tests
        case '*'
            return 1
    end
end

function __git_agent_search_mode_formats
    switch $argv[1]
        case ls
            printf '%s\n' text json
        case ls-remotes
            printf '%s\n' text json completion
        case ls-files
            printf '%s\n' tree json
        case '*'
            printf '%s\n' json brief
    end
end

function __git_agent_search_mode_allows_format
    contains -- "$argv[2]" (__git_agent_search_mode_formats "$argv[1]")
end

function __git_agent_option_state_is_valid
    set -l command_name $argv[1]
    set -l format_value $argv[2]
    set -l enabled_options
    if test "$argv[3]" != __none__
        set enabled_options (string split ',' -- "$argv[3]")
    end
    set -l seen_options $argv[4..-1]

    set -l efforts
    for effort in low medium high xhigh
        contains -- $effort $enabled_options; and set -a efforts $effort
    end
    test (count $efforts) -le 1; or return 1

    switch $command_name
        case review simplify
            if contains -- help $seen_options
                test (count $seen_options) -eq 1; or return 1
            end
            set -l modes
            for mode in codebase uncommitted staged
                contains -- $mode $enabled_options; and set -a modes $mode
            end
            test (count $modes) -le 1; or return 1
            if contains -- wait $seen_options
                test (count $seen_options) -eq 1; or return 1
            end
            if contains -- follow-up $seen_options
                test (count $seen_options) -eq 1; or return 1
            end
            if contains -- depth $seen_options; and contains -- max-steps $seen_options
                return 1
            end
            return 0
        case search
            set -l list_modes
            for mode in ls ls-remotes ls-files
                contains -- $mode $enabled_options; and set -a list_modes $mode
            end
            test (count $list_modes) -le 1; or return 1

            if test (count $list_modes) -eq 1
                for option in $seen_options
                    __git_agent_search_mode_allows_option "$list_modes[1]" "$option"; or return 1
                end
                if test "$format_value" != __unset__
                    __git_agent_search_mode_allows_format "$list_modes[1]" "$format_value"; or return 1
                end
            end
    end
end

function __git_agent_option_is_compatible
    set -l command_name $argv[1]
    set -l candidate $argv[2]
    set -l format_value $argv[3]
    set -l enabled_options
    if test "$argv[4]" != __none__
        set enabled_options (string split ',' -- "$argv[4]")
    end
    set -l seen_options $argv[5..-1]

    if contains -- "$candidate" low medium high xhigh
        for effort in low medium high xhigh
            contains -- $effort $enabled_options; and return 1
        end
    end

    switch $command_name
        case review simplify
            contains -- help $seen_options; and return 1
            if test "$candidate" = help
                test (count $seen_options) -eq 0
                return
            end
            contains -- wait $seen_options; and return 1
            if test "$candidate" = wait
                test (count $seen_options) -eq 0
                return
            end
            contains -- follow-up $seen_options; and return 1
            if test "$candidate" = follow-up
                test (count $seen_options) -eq 0
                return
            end
            if contains -- "$candidate" codebase uncommitted staged
                for mode in codebase uncommitted staged
                    contains -- $mode $enabled_options; and return 1
                end
            end
            if test "$candidate" = depth; and contains -- max-steps $seen_options
                return 1
            end
            if test "$candidate" = max-steps; and contains -- depth $seen_options
                return 1
            end
        case search
            set -l list_modes
            for mode in ls ls-remotes ls-files
                contains -- $mode $enabled_options; and set -a list_modes $mode
            end
            if test (count $list_modes) -eq 1
                __git_agent_search_mode_allows_option "$list_modes[1]" "$candidate"
                return
            end

            if contains -- "$candidate" ls ls-remotes ls-files
                for option in $seen_options
                    __git_agent_search_mode_allows_option "$candidate" "$option"; or return 1
                end
                if test "$format_value" != __unset__
                    __git_agent_search_mode_allows_format "$candidate" "$format_value"; or return 1
                end
                return 0
            end
            if test "$format_value" != __unset__
                contains -- "$format_value" json brief; or return 1
            end
    end

    return 0
end

function __git_agent_option_available
    set -l candidate $argv[1]
    set -l allowed_commands $argv[2..-1]
    set -l words (commandline -opc)
    test (count $words) -gt 1; or return 1

    set -l command_name $words[2]
    if test (count $allowed_commands) -gt 0
        contains -- "$command_name" $allowed_commands; or return 1
    end
    __git_agent_command_has_option "$command_name" "$candidate"; or return 1

    set -l seen_options
    set -l enabled_options
    set -l expecting
    set -l format_value __unset__

    for word in $words[3..-1]
        if test -n "$expecting"
            __git_agent_option_value_is_valid "$command_name" "$expecting" "$word"; or return 1
            if test "$expecting" = format
                set format_value "$word"
            end
            set expecting
            continue
        end

        string match -q -- '-*' "$word"; or return 1
        test "$word" != -; and test "$word" != --; or return 1

        set -l option_token (string replace -r '^-{1,2}' '' -- "$word")
        set -l parts (string split -m 1 '=' -- "$option_token")
        set -l option $parts[1]
        __git_agent_command_has_option "$command_name" "$option"; or return 1
        not contains -- "$option" $seen_options; or return 1
        set -a seen_options "$option"

        if __git_agent_option_takes_value "$option"
            if test (count $parts) -eq 1
                set expecting "$option"
            else
                set -l value $parts[2]
                __git_agent_option_value_is_valid "$command_name" "$option" "$value"; or return 1
                if test "$option" = format
                    set format_value "$value"
                end
            end
        else
            if test (count $parts) -eq 1
                set -a enabled_options "$option"
            else
                __git_agent_bool_value_is_valid "$parts[2]"; or return 1
                __git_agent_bool_value_is_true "$parts[2]"; and set -a enabled_options "$option"
            end
        end
    end

    set -l enabled_key __none__
    if test (count $enabled_options) -gt 0
        set enabled_key (string join ',' $enabled_options)
    end
    __git_agent_option_state_is_valid "$command_name" "$format_value" "$enabled_key" $seen_options; or return 1
    if test -n "$expecting"
        test "$candidate" = "$expecting"
        return
    end
    not contains -- "$candidate" $seen_options; or return 1
    __git_agent_option_is_compatible "$command_name" "$candidate" "$format_value" "$enabled_key" $seen_options
end

function __git_agent_release_note_can_complete
    set -l candidate $argv[1]
    set -l words (commandline -opc)
    test (count $words) -gt 1; and test "$words[2]" = release-note; or return 1

    set -l seen_options
    set -l enabled_options
    set -l expecting
    set -l positionals
    set -l parsing_options 1

    for word in $words[3..-1]
        if test -n "$expecting"
            __git_agent_option_value_is_valid release-note "$expecting" "$word"; or return 1
            set expecting
            continue
        end

        if test $parsing_options -eq 1; and test "$word" = --
            set parsing_options 0
            continue
        end
        if test $parsing_options -eq 1; and string match -q -- '-*' "$word"; and test "$word" != -
            set -l option_token (string replace -r '^-{1,2}' '' -- "$word")
            set -l parts (string split -m 1 '=' -- "$option_token")
            set -l option $parts[1]
            __git_agent_command_has_option release-note "$option"; or return 1
            not contains -- "$option" $seen_options; or return 1
            set -a seen_options "$option"
            if __git_agent_option_takes_value "$option"
                if test (count $parts) -eq 1
                    set expecting "$option"
                else
                    __git_agent_option_value_is_valid release-note "$option" "$parts[2]"; or return 1
                end
            else
                if test (count $parts) -eq 1
                    set -a enabled_options "$option"
                else
                    __git_agent_bool_value_is_valid "$parts[2]"; or return 1
                    __git_agent_bool_value_is_true "$parts[2]"; and set -a enabled_options "$option"
                end
            end
            continue
        end

        set parsing_options 0
        set -a positionals "$word"
    end

    test -z "$expecting"; or return 1
    set -l enabled_key __none__
    if test (count $enabled_options) -gt 0
        set enabled_key (string join ',' $enabled_options)
    end
    __git_agent_option_state_is_valid release-note __unset__ "$enabled_key" $seen_options; or return 1
    switch $candidate
        case bump ref
            test (count $positionals) -eq 0; and return 0
            test "$candidate" = ref; and test (count $positionals) -eq 1; and not contains -- "$positionals[1]" patch minor major
        case '*'
            return 1
    end
end

function __git_agent_search_mode_is_enabled
    set -l mode $argv[1]
    set -l words (commandline -opc)
    for word in $words[3..-1]
        if test "$word" = --$mode; or test "$word" = -$mode
            return 0
        end
        for value in 1 t T TRUE true True
            if test "$word" = --$mode=$value; or test "$word" = -$mode=$value
                return 0
            end
        end
    end
    return 1
end

function __git_agent_search_formats
    if __git_agent_search_mode_is_enabled ls-remotes
        __git_agent_search_mode_formats ls-remotes
    else if __git_agent_search_mode_is_enabled ls-files
        __git_agent_search_mode_formats ls-files
    else if __git_agent_search_mode_is_enabled ls
        __git_agent_search_mode_formats ls
    else
        __git_agent_search_mode_formats
    end
end

function __git_agent_git_refs
    command git for-each-ref --format='%(refname:short)' refs/heads refs/remotes refs/tags 2>/dev/null
    command git rev-parse --short HEAD 2>/dev/null
end

function __git_agent_cached_remotes
    command git-agent search --ls-remotes --format completion 2>/dev/null
end

complete -c git-agent -f

complete -c git-agent -n '__git_agent_no_subcommand' -a commit -d 'Generate a message and commit staged changes'
complete -c git-agent -n '__git_agent_no_subcommand' -a config -d 'Read or update persistent configuration'
complete -c git-agent -n '__git_agent_no_subcommand' -a index -d 'Manage synchronized search indexes'
complete -c git-agent -n '__git_agent_no_subcommand' -a commit-msg -d 'Generate a commit message from staged changes'
complete -c git-agent -n '__git_agent_no_subcommand' -a pr-message -d 'Generate a pull request message from branch changes'
complete -c git-agent -n '__git_agent_no_subcommand' -a release-note -d 'Generate a release note for a range or version bump'
complete -c git-agent -n '__git_agent_no_subcommand' -a review -d 'Review code with structured findings and streamed agent events'
complete -c git-agent -n '__git_agent_no_subcommand' -a search -d 'Search repository context with embeddings'
complete -c git-agent -n '__git_agent_no_subcommand' -a simplify -d 'Find behavior-preserving code simplifications'
complete -c git-agent -n '__git_agent_no_subcommand' -a help -d 'Show usage'

complete -c git-agent -n '__git_agent_config_needs_key' -a index.remote -d 'Dedicated Git remote for synchronized revision indexes'
complete -c git-agent -n '__git_agent_config_can_unset' -l unset -d 'Remove a configuration value'
complete -c git-agent -n '__git_agent_needs_index_subcommand' -a sync -d 'Push all completed local revision indexes'
complete -c git-agent -n '__git_agent_needs_index_subcommand' -a migrate -d 'Migrate synchronized indexes to a newer schema'
complete -c git-agent -n '__git_agent_index_migrate_can_complete to' -l to -r -f -a v2 -d 'Target index schema version'
complete -c git-agent -n '__git_agent_index_migrate_can_complete dry-run' -l dry-run -d 'Report migration size without changing the remote'
complete -c git-agent -n '__git_agent_index_migrate_can_complete option-to' -a '--to' -d 'Target index schema version'
complete -c git-agent -n '__git_agent_index_migrate_can_complete option-dry-run' -a '--dry-run' -d 'Report migration size without changing the remote'

complete -c git-agent -n '__git_agent_option_available amend commit' -l amend -d 'Generate an amended commit message and amend HEAD'
complete -c git-agent -n '__git_agent_option_available amend commit-msg' -l amend -d 'Generate an amended commit message'

complete -c git-agent -n '__git_agent_option_available codebase review simplify' -l codebase -d 'Inspect the full codebase'
complete -c git-agent -n '__git_agent_option_available uncommitted review simplify' -l uncommitted -d 'Inspect all dirty worktree changes'
complete -c git-agent -n '__git_agent_option_available staged review simplify' -l staged -d 'Inspect staged changes only'
complete -c git-agent -n '__git_agent_option_available wait review simplify' -l wait -r -f -d 'Wait for a detached task ID and print its report'
complete -c git-agent -n '__git_agent_option_available follow-up review simplify' -l follow-up -r -f -d 'Re-evaluate a successful provider turn ID'
complete -c git-agent -n '__git_agent_option_available depth review' -l depth -r -f -a 'fast balanced thorough' -d 'Select depth and reasoning default: fast=low, balanced=medium, thorough=high'
complete -c git-agent -n '__git_agent_option_available depth simplify' -l depth -r -f -a 'fast balanced thorough' -d 'Select depth and reasoning default: fast=low, balanced=low, thorough=medium'
complete -c git-agent -n '__git_agent_option_available max-web-searches review simplify' -l max-web-searches -r -f -d 'Cap provider-hosted web searches'
complete -c git-agent -n '__git_agent_option_available dry-run review simplify' -l dry-run -d 'Emit deterministic provider events without a provider request'
complete -c git-agent -n '__git_agent_option_available help review simplify' -l help -d 'Show command help'
complete -c git-agent -n '__git_agent_option_available orchestration-artifact review simplify' -l orchestration-artifact -r -d 'Read helper-authorized orchestration artifact manifest'

complete -c git-agent -n '__git_agent_option_available model commit commit-msg pr-message release-note review simplify' -l model -r -f -d 'Set generation model'
complete -c git-agent -n '__git_agent_option_available fast commit commit-msg pr-message release-note review simplify' -l fast -d 'Use priority service tier'
complete -c git-agent -n '__git_agent_option_available low commit commit-msg pr-message release-note review simplify' -l low -d 'Use low reasoning effort'
complete -c git-agent -n '__git_agent_option_available medium commit commit-msg pr-message release-note review simplify' -l medium -d 'Use medium reasoning effort'
complete -c git-agent -n '__git_agent_option_available high commit commit-msg pr-message release-note review simplify' -l high -d 'Use high reasoning effort'
complete -c git-agent -n '__git_agent_option_available xhigh commit commit-msg pr-message release-note review simplify' -l xhigh -d 'Use xhigh reasoning effort'
complete -c git-agent -n '__git_agent_option_available base-url commit commit-msg pr-message release-note review simplify' -l base-url -r -f -d 'Override provider base URL'
complete -c git-agent -n '__git_agent_option_available timeout commit commit-msg pr-message release-note review simplify' -l timeout -r -f -d 'Set request timeout'
complete -c git-agent -n '__git_agent_option_available max-steps commit commit-msg pr-message release-note review simplify' -l max-steps -r -f -d 'Set maximum agent steps'
complete -c git-agent -n '__git_agent_option_available guidance-family commit commit-msg pr-message release-note review simplify' -l guidance-family -r -f -a 'auto agents claude codex none' -d 'Force guidance family'
complete -c git-agent -n '__git_agent_option_available append-prompt commit commit-msg pr-message release-note review simplify' -l append-prompt -r -f -d 'Append a user prompt hint to the model request'
complete -c git-agent -n '__git_agent_option_available debug commit commit-msg pr-message release-note review simplify' -l debug -d 'Enable debug output on stderr'
complete -c git-agent -n '__git_agent_option_available pprof commit commit-msg pr-message release-note review simplify' -l pprof -r -f -d 'Serve pprof on address'

complete -c git-agent -n '__git_agent_option_available out release-note' -l out -r -d 'Write release note markdown to file'
complete -c git-agent -n '__git_agent_release_note_can_complete bump' -a 'patch minor major' -d 'Infer release version from latest semver tag'
complete -c git-agent -n '__git_agent_release_note_can_complete ref' -a '(__git_agent_git_refs)' -d 'Git ref'

complete -c git-agent -n '__git_agent_option_available rev search' -l rev -r -f -a '(__git_agent_git_refs)' -d 'Search a committed Git tree'
complete -c git-agent -n '__git_agent_option_available remote search' -l remote -r -f -a '(__git_agent_cached_remotes)' -d 'Search a cached remote Git repository URL'
complete -c git-agent -n '__git_agent_option_available scope search' -l scope -r -d 'Comma-separated relative paths to search or index'
complete -c git-agent -n '__git_agent_option_available min-score search' -l min-score -r -f -d 'Minimum final hybrid score'
complete -c git-agent -n '__git_agent_option_available limit search' -l limit -r -f -d 'Maximum results'
complete -c git-agent -n '__git_agent_option_available format search' -l format -r -f -a '(__git_agent_search_formats)' -d 'Output format'
complete -c git-agent -n '__git_agent_option_available index search' -l index -d 'Build embeddings without searching'
complete -c git-agent -n '__git_agent_option_available reindex search' -l reindex -d 'Rebuild embeddings for the selected source'
complete -c git-agent -n '__git_agent_option_available code search' -l code -d 'Search code files only'
complete -c git-agent -n '__git_agent_option_available no-tests search' -l no-tests -d 'Exclude common test files and test directories from results and ls-files output'
complete -c git-agent -n '__git_agent_option_available agent search' -l agent -d 'Serve indexing progress on localhost when embeddings need work'
complete -c git-agent -n '__git_agent_option_available ls search' -l ls -d 'List search indexes for the current project or remote'
complete -c git-agent -n '__git_agent_option_available ls-remotes search' -l ls-remotes -d 'List cached remote repositories'
complete -c git-agent -n '__git_agent_option_available ls-files search' -l ls-files -d 'List indexed files from a search index as a tree'
complete -c git-agent -n '__git_agent_option_available embedding-model search' -l embedding-model -r -f -a 'text-embedding-3-small text-embedding-3-large' -d 'Embedding model'
complete -c git-agent -n '__git_agent_option_available embedding-dimensions search' -l embedding-dimensions -r -f -a '512 768 1024 1536 3072' -d 'Embedding dimensions'
complete -c git-agent -n '__git_agent_option_available base-url search' -l base-url -r -f -d 'Override provider base URL'
complete -c git-agent -n '__git_agent_option_available timeout search' -l timeout -r -f -d 'Override default request timeout'
complete -c git-agent -n '__git_agent_option_available debug search' -l debug -d 'Enable debug output on stderr'
complete -c git-agent -n '__git_agent_option_available pprof search' -l pprof -r -f -d 'Serve pprof on address'
