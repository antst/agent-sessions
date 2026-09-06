"use strict";

const { isDeepStrictEqual } = require("node:util");
const schema = require("../../internal/protocol/session.schema.json");
const definitions = schema.$defs;

function validate(name, value) { return !validationError(name, value); }

function validationError(name, value) {
  if (!definitions[name]) return new Error("invalid session value");
  const problem = issue(definitions[name], value, "", "");
  return problem ? new Error(`${name}: ${problem}`) : undefined;
}

function issue(rule, value, path, withName) {
  if (rule.$ref) return issue(definitions[rule.$ref.slice("#/$defs/".length)], value, path, withName);
  if (rule.type && !validType(rule.type, value)) return `${location(path)} must be ${rule.type}`;
  if (Object.hasOwn(rule, "const") && !isDeepStrictEqual(rule.const, value)) return `${location(path)} must equal ${String(rule.const)}`;
  if (rule.enum && !rule.enum.some((item) => isDeepStrictEqual(item, value))) return `${location(path)} must be one of the allowed values`;
  if (typeof value === "string") {
    const length = [...value].length;
    if (rule.minLength > length) return `${location(path)} must contain at least ${rule.minLength} character${rule.minLength === 1 ? "" : "s"}`;
    if (rule.maxLength < length) return `${location(path)} must contain at most ${rule.maxLength} character${rule.maxLength === 1 ? "" : "s"}`;
    if (rule.pattern && !new RegExp(rule.pattern, "u").test(value)) return `${location(path)} must match ${JSON.stringify(rule.pattern)}`;
  }
  if (typeof value === "number") {
    if (rule.minimum > value) return `${location(path)} must be at least ${rule.minimum}`;
    if (rule.exclusiveMinimum >= value) return `${location(path)} must be greater than ${rule.exclusiveMinimum}`;
  }
  if (isObject(value)) {
    for (const name of rule.required || []) if (!Object.hasOwn(value, name)) return `${location(childPath(path, name))} is required`;
    if (Object.keys(value).length < (rule.minProperties || 0)) return `${location(path)} must contain at least ${rule.minProperties} properties`;
    for (const [name, item] of Object.entries(value)) {
      const child = rule.properties?.[name];
      if (!child && rule.additionalProperties === false) return `${location(childPath(path, name))} is not allowed`;
      if (child) { const problem = issue(child, item, childPath(path, name), withName); if (problem) return problem; }
    }
  }
  if (Array.isArray(value)) {
    if (rule.items) for (let index = 0; index < value.length; index++) { const problem = issue(rule.items, value[index], `${path}[${index}]`, withName); if (problem) return problem; }
    if (rule.uniqueItems) for (let index = 0; index < value.length; index++) if (value.slice(0, index).some((other) => isDeepStrictEqual(other, value[index]))) return `${location(`${path}[${index}]`)} must not duplicate an earlier item`;
  }
  if (rule.allOf) for (const child of rule.allOf) { const problem = issue(child, value, path, withName); if (problem) return problem; }
  if (rule.not && !issue(rule.not, value, path, "")) {
    const [name] = rule.not.required || [];
    if (name) return `${location(childPath(path, name))} is not allowed${withName ? ` with ${JSON.stringify(withName)}` : ""}`;
    return `${location(path)} violates an excluded constraint`;
  }
  if (rule.if) {
    const matched = !issue(rule.if, value, path, "");
    const branch = rule[matched ? "then" : "else"];
    if (branch) return issue(branch, value, path, matched && rule.if.required?.length === 1 ? rule.if.required[0] : withName);
  }
  return "";
}

function validType(type, value) {
  if (type === "array") return Array.isArray(value);
  if (type === "object") return isObject(value);
  if (type === "integer") return Number.isSafeInteger(value);
  return typeof value === type;
}

function isObject(value) { return value !== null && typeof value === "object" && !Array.isArray(value); }
function childPath(path, name) { return path ? `${path}.${name}` : name; }
function location(path) { return path ? JSON.stringify(path) : "$"; }
function encode(name, value) { const raw = JSON.stringify(value); if (raw === undefined) throw new Error(`${name}: $ cannot be encoded as JSON`); const error = validationError(name, JSON.parse(raw)); if (error) throw error; return raw; }

module.exports = { encode, schema, validate, validationError };
