Extend math_ops with two more operations, each in its own module with its own unit tests (use Python's built-in unittest, runnable with `python3 -m unittest discover -s . -p 'test_*.py'`):

1. `sub.py` with `subtract_numbers(a, b)` returning a - b.
2. `div.py` with `divide_numbers(a, b)` returning a / b, raising ValueError("division by zero") when b == 0.

Keep add.py unchanged.
