Classify whether the user message is a refinement of a specific running
mission or a new request. A refinement narrows or redirects what the
mission is currently doing (e.g. "focus only on high-severity", "use
the 2024 data"). Meta-action requests about a mission — cancel, stop,
abandon, pause, resume, or inspect — are NOT refinements and must
return match=null so the coordinator can act on them itself.

Reply ONLY with JSON of the shape
{"match": <integer id from the list or null>, "confidence": <0.0-1.0>}.
Set `match` to null when unsure, when the message is clearly a new
topic, or when it is a meta-action (cancel / stop / status / etc).
