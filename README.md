# TestProxy
This little HTTP proxy forwards the request to two different upstreams.
The primary upstream's response is sent back to the client, and
the second upstream's response is compared with the first upstream's response.

If the status code's differ, then the request and both responses are logged
to three files under a subdir.


## Ratinale
I want to rewrite my Python server in Go, but this is mission-critical service,
so the rewrite must be as good as possible. And it has to mimic the quirks of
the legacy app...
So I need to torture test it as much as possible.
