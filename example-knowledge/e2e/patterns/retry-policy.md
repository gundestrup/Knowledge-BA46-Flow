# Retry policy

A retry is permitted only when a request fails before a successful response.
The client uses bounded exponential backoff: wait one, two, then four seconds.
Stop after three attempts and keep the original error visible to the operator.
Never retry an operation after a successful response has been received.

The caller must include the request identifier in every attempt log.
