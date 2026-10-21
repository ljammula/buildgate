'use strict';

const isNumber = require('is-number');

// double returns twice value when it is a number.
function double(value) {
  if (!isNumber(value)) {
    throw new TypeError('not a number');
  }
  return Number(value) * 2;
}

// triple is not yet implemented.
function triple(value) {
  throw new Error('not yet implemented');
}

module.exports = { double, triple };
