# argv[1] is a uuid, a session_ id or a claude.ai/code URL; tab offers the uuids.
complete -c devresume -f
complete -c devresume -n __fish_is_first_arg -a '(__dev_complete_sessions)'
