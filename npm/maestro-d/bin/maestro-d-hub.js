#!/usr/bin/env node
const { hubBinary } = require('../dist/cjs/binary.js')
require('./run.js')('maestro-d-hub', hubBinary)
