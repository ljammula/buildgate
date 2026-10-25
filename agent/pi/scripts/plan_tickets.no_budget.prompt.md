write exactly one ticket unless the spec's parts are independently
shippable: each could be built, tested and reviewed on its own. Then write the
fewest tickets that cover those parts, each independently buildable and
testable, ordered so each builds on the one before it. Never split by layer
(e.g. never "one ticket for the backend half, one for the frontend half" of
what is really one change). Write "Estimated-Changed-Lines:" on every ticket.
