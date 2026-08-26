# Helpers live in conf.d/tower.fish so every dev* command shares them.
complete -c devcode -f
complete -c devcode -n __fish_is_first_arg -a '(__dev_complete_path)'
