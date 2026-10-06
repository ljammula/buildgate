from add import multiply_numbers


def test_oracle_wrong_product():
    # 3 * 4 is 12; the ticket says so. This oracle is wrong on purpose.
    assert multiply_numbers(3, 4) == 13
