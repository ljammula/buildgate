MUTS=[
("no-trim","strings.ToLower(strings.TrimSpace(state))","strings.ToLower(state)"),
("open-inverted","return !t.Done }","return t.Done }"),
("aliases-input","make([]Todo, 0, len(todos))","todos[:0]"),
("error-unquoted-normalized","fmt.Errorf(\"unknown state %q\", state)","fmt.Errorf(\"unknown state %q\", normalized)"),
]
