"use strict";

const { isDeepStrictEqual } = require("node:util");
const schema = require("../../internal/protocol/session.schema.json");
const definitions = schema.$defs;

function validate(name, value) { return !!definitions[name] && valid(definitions[name], value); }

function valid(rule, value) {
  if (rule.$ref) return valid(definitions[rule.$ref.slice("#/$defs/".length)], value);
  if (rule.type && !validType(rule.type, value)) return false;
  if (Object.hasOwn(rule, "const") && !isDeepStrictEqual(rule.const, value)) return false;
  if (rule.enum && !rule.enum.some((item) => isDeepStrictEqual(item, value))) return false;
  if (typeof value === "string" && (rule.minLength > [...value].length || rule.maxLength < [...value].length)) return false;
  if (typeof value === "number" && (rule.minimum > value || rule.exclusiveMinimum >= value)) return false;
  if (isObject(value) && !validObject(rule, value)) return false;
  if (Array.isArray(value) && (!value.every((item) => !rule.items || valid(rule.items, item)) || rule.uniqueItems && value.some((item, index) => value.slice(0, index).some((other) => isDeepStrictEqual(item, other))))) return false;
  if (rule.allOf && !rule.allOf.every((child) => valid(child, value))) return false;
  if (rule.not && valid(rule.not, value)) return false;
  if (rule.if && rule[valid(rule.if, value) ? "then" : "else"] && !valid(rule[valid(rule.if, value) ? "then" : "else"], value)) return false;
  return true;
}

function validObject(rule, value) {
  if ((rule.required || []).some((name) => !Object.hasOwn(value, name)) || Object.keys(value).length < (rule.minProperties || 0)) return false;
  return Object.entries(value).every(([name, item]) => rule.properties?.[name] ? valid(rule.properties[name], item) : rule.additionalProperties !== false);
}

function validType(type, value) {
  if (type === "array") return Array.isArray(value);
  if (type === "object") return isObject(value);
  if (type === "integer") return Number.isSafeInteger(value);
  return typeof value === type;
}

function isObject(value) { return value !== null && typeof value === "object" && !Array.isArray(value); }
function encode(name, value) { const raw = JSON.stringify(value); if (raw === undefined || !validate(name, JSON.parse(raw))) throw new Error(`invalid ${name}`); return raw; }

module.exports = { encode, schema, validate };
