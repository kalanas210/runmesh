package ratelimit

// tokenBucketScript is the whole of the rate limiter's shared state logic,
// run atomically inside Redis via EVAL rather than as a read-modify-write
// from this process. That atomicity is the entire reason for a script: two
// RunMesh replicas racing a plain GET/compute/SET over the same bucket key
// would both read the same starting balance and both believe they spent the
// last token, which is exactly the over-admission a rate limiter exists to
// prevent.
//
// KEYS[1]: the bucket key.
// ARGV[1]: capacity — the bucket's burst size.
// ARGV[2]: refill rate, in tokens per second.
//
// Returns a two-element array: {allowed, retry_after_ms}. allowed is 1 or 0.
// retry_after_ms is 0 when allowed is 1, and otherwise an ESTIMATE of when
// the bucket would next have a token if nothing else spends one first — not
// a promise, because a concurrent caller can still race for that same token.
// checkRateLimit (internal/engine/ratelimit.go) passes it to
// runmesh.RetryIn regardless, the same way a server's own Retry-After is
// trusted there: a slightly optimistic wait is still better information than
// the engine's own backoff curve, which knows nothing about this bucket at
// all.
const tokenBucketScript = `
local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])

-- redis.call('TIME') is the clock, not this call's own arrival time at the
-- server: every replica computing elapsed time against its OWN wall clock
-- would let clock skew between RunMesh instances turn into rate-limit skew,
-- and Redis's TIME is the one clock every replica already agrees on, because
-- they are all asking the same server for it.
local t = redis.call('TIME')
local now_ms = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

local bucket = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(bucket[1])
local ts = tonumber(bucket[2])
if tokens == nil then
  -- No prior state: a bucket starts full, not empty, so the FIRST request a
  -- fresh key ever sees is never itself a false refusal.
  tokens = capacity
  ts = now_ms
end

local elapsed_ms = now_ms - ts
if elapsed_ms < 0 then
  elapsed_ms = 0 -- a clock that moved backwards costs a lost refill, never a negative one
end
tokens = math.min(capacity, tokens + (elapsed_ms / 1000.0) * rate)

local allowed = 0
local retry_after_ms = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry_after_ms = math.ceil(((1 - tokens) / rate) * 1000)
end

redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', tostring(now_ms))
-- Long enough for the bucket to fully refill from empty, plus a minute of
-- slack for a burst of activity right at that boundary — an idle bucket must
-- not live in Redis forever, and a TTL is cheaper than a sweep.
redis.call('EXPIRE', KEYS[1], math.ceil(capacity / rate) + 60)

return {allowed, retry_after_ms}
`
