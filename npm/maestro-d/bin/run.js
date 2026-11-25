const { spawnSync } = require('node:child_process')
const { constants } = require('node:os')

module.exports = function run(name, locate) {
  let bin
  try {
    bin = locate()
  } catch (err) {
    console.error(`${name}: ${err.message}`)
    process.exit(3)
  }

  const result = spawnSync(bin, process.argv.slice(2), { stdio: 'inherit' })

  if (result.error) {
    console.error(`${name}: ${result.error.message}`)
    process.exit(3)
  }
  // A binary killed by a signal has no exit code; report it the way a shell would.
  if (result.signal) {
    process.exit(128 + constants.signals[result.signal])
  }
  process.exit(result.status === null ? 1 : result.status)
}
