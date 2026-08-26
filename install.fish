#!/usr/bin/env fish
# Symlink the fish integration into ~/.config/fish.
#
# Symlinks rather than copies, so editing the file you use is editing the file
# you commit. Existing files are moved aside to <name>.pre-install, never
# overwritten. Run with --uninstall to remove the links again.

argparse u/uninstall h/help -- $argv; or exit 1

if set -q _flag_help
    echo "usage: ./install.fish [--uninstall]"
    exit 0
end

set -l src (realpath (dirname (status filename)))/fish
set -l dst $HOME/.config/fish

if not test -d $src
    echo "install: $src not found, run this from inside the repo" >&2
    exit 1
end

set -l files conf.d/tower.fish
for f in $src/completions/*.fish
    set -a files completions/(basename $f)
end

for f in $files
    set -l target $dst/$f
    if set -q _flag_uninstall
        if test -L $target
            rm $target
            echo "removed  $target"
        end
        continue
    end

    mkdir -p (dirname $target)
    if test -L $target
        rm $target
    else if test -e $target
        mv $target $target.pre-install
        echo "kept     $target.pre-install"
    end
    ln -s $src/$f $target
    echo "linked   $target"
end

if not set -q _flag_uninstall
    echo
    echo "open a new shell (or: exec fish) to load them"
end
