# argv[1] is a session uuid (not completable), argv[2] is the directory.
complete -c devresume -f
complete -c devresume -n '__fish_is_nth_token 2' -a '(__dev_complete_path)'
