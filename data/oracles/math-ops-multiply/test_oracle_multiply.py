from add import multiply_numbers


def test_oracle_positive_product():
    assert multiply_numbers(3, 4) == 12


def test_oracle_negative_times_positive():
    assert multiply_numbers(-2, 5) == -10


def test_oracle_zero_and_float():
    assert multiply_numbers(0, 9) == 0
    assert abs(multiply_numbers(1.5, 2) - 3.0) < 1e-9
