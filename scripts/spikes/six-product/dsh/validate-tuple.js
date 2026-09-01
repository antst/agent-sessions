#!/usr/bin/env node

import fs from 'node:fs'

const [profilePackagePath, acpPackagePath, pluginPackagePath, outputPath] = process.argv.slice(2)
if (![profilePackagePath, acpPackagePath, pluginPackagePath, outputPath].every(Boolean)) {
  throw new Error('usage: validate-tuple.js PROFILE_PACKAGE ACP_PACKAGE PLUGIN_PACKAGE OUTPUT')
}

const expected = '0.1.2-alpha.3'
const read = (file) => JSON.parse(fs.readFileSync(file, 'utf8'))
const actual = {
  profile: read(profilePackagePath).version,
  acp_app: read(acpPackagePath).version,
  plugin: read(pluginPackagePath).version,
}
const validate = (candidate) => Object.entries(candidate)
  .filter(([, version]) => version !== expected)
  .map(([member, version]) => ({ member, version, expected }))

const exactErrors = validate(actual)
const mismatchCandidate = { ...actual, cli: '0.1.1-rc.2' }
const mismatchErrors = validate(mismatchCandidate)
const result = {
  expected,
  actual,
  exact_tuple_ok: exactErrors.length === 0,
  exact_errors: exactErrors,
  mismatch_candidate: mismatchCandidate,
  mismatch_rejected: mismatchErrors.some((error) => error.member === 'cli'),
  mismatch_errors: mismatchErrors,
  policy: 'all tuple members must equal the pinned version; no semver widening',
}
fs.writeFileSync(outputPath, `${JSON.stringify(result, null, 2)}\n`, { mode: 0o600 })
if (!result.exact_tuple_ok || !result.mismatch_rejected) process.exitCode = 1
