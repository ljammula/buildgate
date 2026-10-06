This is acceptance criterion {index} of {total} for this request -- the ONLY
one you are drafting in this invocation. The other {total} criteria (including
criterion {index} itself, elsewhere) are each drafted in their OWN separate,
isolated pi invocation that cannot see this one's output, so if every
invocation defaults to the same file name they will collide once the host
assembles them together. Name your oracle file exactly {oracle_filename} --
not {default_filename} unless {index} is 1. If you set target_path, its file
name (the last path component) MUST be exactly {oracle_filename} too, in
whatever directory the code it tests actually lives in: a target_path whose
file name differs from {oracle_filename} will be REJECTED and this criterion
dropped, because the host has no way to tell which file you meant it for.
