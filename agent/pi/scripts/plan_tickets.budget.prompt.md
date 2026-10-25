estimate the whole change in changed lines (insertions plus deletions, tests
included) before you split anything. If it is within {budget} lines, write
exactly one ticket, however many packages it touches. If it is over {budget}
lines, split it into the fewest tickets that each fit within {budget} lines,
each one independently buildable and testable, ordered so each builds on the
one before it. Never split by layer (e.g. never "one ticket for the backend
half, one for the frontend half" of what is really one change). Write
"Estimated-Changed-Lines:" on every ticket.
