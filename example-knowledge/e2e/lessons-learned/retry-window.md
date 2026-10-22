# Retry window

A retry is permitted only when a request fails before a successful response.
The client uses bounded exponential backoff: wait one, two, then four seconds.
Stop after three attempts and keep the original error visible to the operator.
Never retry an operation after a successful response has been received.

When the upstream sends a Retry-After header, honor it but keep the retry count
bounded. Record the final attempt count in the request log.
