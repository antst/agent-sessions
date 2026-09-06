#!/usr/bin/env node

import { closeSync, existsSync, fsyncSync, lstatSync, mkdirSync, openSync, readFileSync, renameSync, statSync, unlinkSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { applyEdits, createScanner, findNodeAtLocation, modify, parse, parseTree, printParseErrorCode, SyntaxKind } from "jsonc-parser";

const packageRoot = path.dirname(fileURLToPath(import.meta.url));
const packageVersion = JSON.parse(readFileSync(path.join(packageRoot, "package.json"), "utf8")).version;
const defaultSpecifier = `@sessionbus/opencode@${packageVersion}`;
const configNames = ["opencode.jsonc", "opencode.json", "config.json"];
const formatting = { formattingOptions: { insertSpaces: true, tabSize: 2, eol: "\n" } };

function configFiles(options) {
  const root = options.directory || path.join(options.environment?.XDG_CONFIG_HOME || process.env.XDG_CONFIG_HOME || path.join(os.homedir(), ".config"), "opencode");
  return configNames.map((name) => path.join(root, name));
}

function text(value) {
  return typeof value === "string" && value.length > 0 && !/[\0\r\n]/u.test(value);
}

function filePackageName(specifier) {
  if (!specifier.startsWith("file:")) return "";
  try {
    const target = fileURLToPath(specifier);
    return lstatSync(target).isDirectory() ? JSON.parse(readFileSync(path.join(target, "package.json"), "utf8")).name : "";
  } catch {
    return "";
  }
}

function ownedSpecifier(specifier) {
  if (typeof specifier !== "string") return false;
  if (specifier === "@sessionbus/opencode" || specifier.startsWith("@sessionbus/opencode@")) return true;
  if (filePackageName(specifier) === "@sessionbus/opencode") return true;
  if (specifier.startsWith("file:")) {
    try { if (/^sessionbus-opencode-[^/]+\.tgz$/u.test(path.basename(fileURLToPath(specifier)))) return true; } catch {}
  }
  return /^https?:\/\/[^\s]*(?:%40|@)sessionbus(?:%2F|\/)opencode(?:@|%40)/u.test(specifier);
}

function entrySpecifier(entry) {
  if (text(entry)) return entry;
  if (Array.isArray(entry) && entry.length === 2 && text(entry[0])) return entry[0];
  return "";
}

function readConfig(file) {
  const body = readFileSync(file, "utf8");
  const errors = [];
  const value = parse(body, errors, { allowTrailingComma: true });
  if (errors.length) throw new Error(`OpenCode config is not valid JSONC: ${file}: ${printParseErrorCode(errors[0].error)}`);
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`OpenCode config must be an object: ${file}`);
  if (value.plugin !== undefined && (!Array.isArray(value.plugin) || value.plugin.some((entry) => !entrySpecifier(entry)))) throw new Error(`OpenCode config plugin must contain strings or [string, options] tuples: ${file}`);
  return { file, body, value, tree: parseTree(body, [], { allowTrailingComma: true }), mode: statSync(file).mode & 0o777 };
}

function commaOffset(body, start, end) {
  const scanner = createScanner(body, false);
  scanner.setPosition(start);
  for (let token = scanner.scan(); token !== SyntaxKind.EOF && scanner.getTokenOffset() < end; token = scanner.scan()) {
    if (token === SyntaxKind.CommaToken) return scanner.getTokenOffset();
  }
  throw new Error("OpenCode config plugin array has no expected comma");
}

function edit(document, specifier, remove) {
  const entries = document.value.plugin || [];
  const indices = entries.flatMap((entry, index) => ownedSpecifier(entrySpecifier(entry)) ? [index] : []);
  let body = document.body;
  const plugin = findNodeAtLocation(document.tree, ["plugin"]);
  if (indices.length) {
    const removed = new Set(indices);
    const survivors = entries.flatMap((_, index) => removed.has(index) ? [] : [index]);
    const edits = indices.map((index) => ({ offset: plugin.children[index].offset, length: plugin.children[index].length, content: "" }));
    for (let index = 0; index + 1 < entries.length; index++) {
      if (survivors.includes(index) && survivors.some((candidate) => candidate > index)) continue;
      edits.push({ offset: commaOffset(body, plugin.children[index].offset + plugin.children[index].length, plugin.children[index + 1].offset), length: 1, content: "" });
    }
    body = applyEdits(body, edits);
  }
  const remaining = entries.length - indices.length;
  if (!remove && document.selected) {
    body = applyEdits(body, plugin ? modify(body, ["plugin", -1], specifier, {}) : modify(body, ["plugin"], [specifier], formatting));
  }
  return body;
}

function atomicCommit(changes) {
  const staged = [];
  try {
    for (const change of changes) {
      mkdirSync(path.dirname(change.file), { recursive: true });
      const suffix = `${process.pid}.${Date.now()}.${staged.length}`;
      const item = { ...change, temporary: `${change.file}.${suffix}.new`, backup: `${change.file}.${suffix}.old`, existed: existsSync(change.file), backedUp: false, installed: false };
      const descriptor = openSync(item.temporary, "wx", change.mode || 0o600);
      try { writeFileSync(descriptor, change.body); fsyncSync(descriptor); } finally { closeSync(descriptor); }
      staged.push(item);
    }
    for (const item of staged) {
      if (item.existed) { renameSync(item.file, item.backup); item.backedUp = true; }
      renameSync(item.temporary, item.file);
      item.installed = true;
    }
  } catch (error) {
    const failures = [error];
    for (const item of staged.reverse()) {
      try {
        if (item.installed && item.existed) renameSync(item.backup, item.file);
        else if (item.installed) unlinkSync(item.file);
        else if (item.backedUp) renameSync(item.backup, item.file);
      } catch (rollback) { failures.push(rollback); }
      try { unlinkSync(item.temporary); } catch (cleanup) { if (cleanup.code !== "ENOENT") failures.push(cleanup); }
    }
    throw failures.length === 1 ? error : new AggregateError(failures, "OpenCode config transaction rollback failed");
  }
  for (const item of staged) if (item.backedUp) unlinkSync(item.backup);
}

export function configure(options = {}) {
  const remove = options.remove === true;
  const specifier = options.specifier || defaultSpecifier;
  if (!remove && (!text(specifier) || !(/^file:/u.test(specifier) || /^https?:\/\//u.test(specifier) || /^@sessionbus\/opencode@[^\s]+$/u.test(specifier)))) throw new Error("OpenCode plugin specifier is invalid");
  const candidates = configFiles(options);
  const existing = candidates.filter(existsSync);
  if (remove && existing.length === 0) return false;
  const selected = existing[0] || candidates[0];
  const documents = existing.map(readConfig);
  if (!existing.length) documents.push({ file: selected, body: "{}\n", value: {}, mode: 0o600 });
  for (const document of documents) document.selected = document.file === selected;
  const owned = documents.flatMap((document) => (document.value.plugin || []).filter((entry) => ownedSpecifier(entrySpecifier(entry))).map((entry) => ({ document, entry })));
  if (remove && owned.length === 0) return false;
  if (!remove && owned.length === 1 && owned[0].document.selected && owned[0].entry === specifier) return false;
  const changes = documents.map((document) => ({ file: document.file, body: edit(document, specifier, remove), mode: document.mode })).filter((change, index) => change.body !== documents[index].body);
  (options.commit || atomicCommit)(changes);
  return changes.length > 0;
}

function argumentsFrom(argv) {
  if (argv.length === 0) return {};
  if (argv.length === 1 && argv[0] === "--remove") return { remove: true };
  if (argv.length === 2 && argv[0] === "--specifier") return { specifier: argv[1] };
  throw new Error("usage: sessionbus-opencode-install [--specifier <exact-package-specifier> | --remove]");
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { configure(argumentsFrom(process.argv.slice(2))); } catch (error) { process.stderr.write(`${error.message}\n`); process.exitCode = 1; }
}
