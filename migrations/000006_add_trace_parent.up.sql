-- The W3C traceparent of the API request that created the row (ADR-0021). Each delivery
-- attempt starts its own trace and links to this one, so a trace survives the trip
-- through the queue. NULL when the request was not traced.
ALTER TABLE notifications ADD COLUMN trace_parent text;
