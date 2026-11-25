#!/usr/bin/env node
const { binaryPath } = require('../dist/cjs/binary.js')
require('./run.js')('maestro-d', binaryPath)
