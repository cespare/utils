# barclock

I use barclock as a replacement for the waybar clock module.

The formatting that the waybar clock module uses puts an extra padding space
before a one-digit day-of-month:

    May  5 21:42     # waybar clock's date format does this for %e
    May 5 21:42      # but I want this

Additionally, since I use both a local and UTC clock, I take the opportunity to
show a more compact display by eliding the day from the UTC clock if it's the
same as the local clock.
