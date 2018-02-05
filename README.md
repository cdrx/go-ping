# go-ping
[![GoDoc](https://godoc.org/github.com/getlantern/go-ping?status.svg)](https://godoc.org/github.com/sparrc/go-ping)

ICMP Ping library for Go, inspired by
[go-fastping](https://github.com/levenlabs/go-fastping)

Here is a very simple example that sends & receives 3 packets:

```go
pinger, err := ping.NewPinger(false)
if err != nil {
  panic(err)
}

stats, err := pinger.ping("www.google.com", 3, 1*time.Second, 10 * time.Second)
if err != nil {
  panic(err)
}
```

## Additional Features

* Low CPU usage
* Allows pinging different destinations in parallel

## Installation:

```
go get github.com/getlantern/go-ping
```

## Note on Linux Support:

This library attempts to send an
"unprivileged" ping via UDP. On linux, this must be enabled by setting

```
sudo sysctl -w net.ipv4.ping_group_range="0   2147483647"
```

If you do not wish to do this, you can set `pinger.SetPrivileged(true)` and
use setcap to allow your binary using go-ping to bind to raw sockets
(or just run as super-user):

```
setcap cap_net_raw=+ep /bin/goping-binary
```

See [this blog](https://sturmflut.github.io/linux/ubuntu/2015/01/17/unprivileged-icmp-sockets-on-linux/)
and [the Go icmp library](https://godoc.org/golang.org/x/net/icmp) for more details.
