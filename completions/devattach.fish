# argv[1] is a session id or claude.ai url, argv[2] is the directory.
complete -c devattach -f
complete -c devattach -s f -l force -d 'Stop the spawner without asking'
complete -c devattach -n '__fish_is_nth_token 2' -a '(__dev_complete_path)'
