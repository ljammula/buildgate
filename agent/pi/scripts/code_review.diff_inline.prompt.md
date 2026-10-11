The diff below is complete for this ticket -- do NOT re-run `git diff` or `git show` to fetch it again. Open other files only when needed to confirm a suspected defect, and keep tool use to a minimum.

On a hunk line `@@ -a,b +c,d @@ text`, the text after the second `@@` is git's guess at the enclosing function or heading, copied from an earlier line of the file. It is not part of the change and can name the wrong section. When a verdict depends on where in a file a change sits, open the file and read the lines around it.

{diff}
