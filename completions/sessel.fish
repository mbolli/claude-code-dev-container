# Subcommands, then session ids (uuids, described by title) where one is taken.
complete -c sessel -f
complete -c sessel -n __fish_use_subcommand -a ls -d 'table of sessions'
complete -c sessel -n __fish_use_subcommand -a open -d 'resume, attach, or move into tmux'
complete -c sessel -n __fish_use_subcommand -a peek -d 'what it was about, where it stopped'
complete -c sessel -n __fish_use_subcommand -a rename -d 'set a title'
complete -c sessel -n __fish_use_subcommand -a rm -d 'delete for real'
complete -c sessel -n __fish_use_subcommand -a resolve -d 'uuid, cwd, state for an id'
complete -c sessel -n __fish_use_subcommand -a new -d 'new project in /develop, served to the phone'
complete -c sessel -n __fish_use_subcommand -a serve -d 'spawners for the phone'
complete -c sessel -n __fish_use_subcommand -a upgrade -d 'restart idle spawners and sessions on the newest claude'
complete -c sessel -n __fish_use_subcommand -a json -d 'every session as JSON'
complete -c sessel -n '__fish_seen_subcommand_from open peek rename rm resolve' -a '(__dev_complete_sessions)'
