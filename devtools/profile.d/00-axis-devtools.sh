# Shown on interactive DevTools login (PTY / SSH). Skip scripts and pipes.
case $- in
*i*) ;;
*) return 0 ;;
esac

[ -t 1 ] || return 0

alias motd='cat /etc/motd'

# Fresh screen so the MOTD is the first thing you see.
clear

[ -r /etc/motd ] && cat /etc/motd
