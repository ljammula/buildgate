'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { double } = require('./index.js');

test('double', () => {
  assert.strictEqual(double(2), 4);
  assert.strictEqual(double('3'), 6);
  assert.throws(() => double('x'), TypeError);
});
