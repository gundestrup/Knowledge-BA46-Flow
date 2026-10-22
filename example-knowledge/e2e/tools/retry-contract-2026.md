---
title: Retry contract as of 2026
tags: [fixture, retry]
published: "2026-10-19"
---

# Retry contract as of 2026

Effective 2026-10-19. The client records the request identifier, operation
name, upstream host, and attempt number before sending each request. A timeout
does not reveal whether the upstream committed the operation. The operator uses
this contract when an upstream response cannot be confirmed. The client keeps
each failure visible and writes every attempt to the operation log. The retry
window is bounded and every outcome is recorded for later review.

When a timeout occurs, never retry automatically after a timeout, even when the
upstream result is unknown. A human reviews the log after the bounded attempts
are complete.
