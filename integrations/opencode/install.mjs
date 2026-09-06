#!/usr/bin/env node

import { closeSync, existsSync, fsyncSync, lstatSync, mkdirSync, openSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const packageRoot = path.dirname(fileURLToPath(import.meta.url));
const packageVersion = JSON.parse(readFileSync(path.join(packageRoot, "package.json"), "utf8")).version;
const defaultSpecifier = `@sessionbus/opencode@${packageVersion}`;

function configPath(environment = process.env) {
  const root = environment.XDG_CONFIG_HOME || path.join(os.homedir(), ".config");
  return path.join(root, "opencode", "opencode.json");
}

function text(value) {
  return typeof value === "string" && value.length > 0 && !/[\0\r\n]/u.test(value);
}

function filePackageName(specifier) {
  if (!specifier.startsWith("file:")) return "";
  try {
    const target = fileURLToPath(specifier);
    const file = lstatSync(target).isDirectory() ? path.join(target, "package.json") : "";
    return file ? JSON.parse(readFileSync(file, "utf8")).name : "";
  } catch {
    return "";
  }
}

function filePackageArchive(specifier) {
  if (!specifier.startsWith("file:")) return false;
  try {
    return /^sessionbus-opencode-[^/]+\.tgz$/u.test(path.basename(fileURLToPath(specifier)));
  } catch {
    return false;
  }
}

function owned(specifier) {
  return typeof specifier === "string" && (
    specifier === "@sessionbus/opencode" ||
    specifier.startsWith("@sessionbus/opencode@") ||
    filePackageName(specifier) === "@sessionbus/opencode" ||
    filePackageArchive(specifier) ||
    /^https?:\/\/[^\s]*(?:%40|@)sessionbus(?:%2F|\/)(?:opencode)(?:@|%40)/u.test(specifier)
  );
}

function decode(file) {
  if (!existsSync(file)) return {};
  if (!lstatSync(file).isFile()) throw new Error(`OpenCode config is not a regular file: ${file}`);
  let value;
  try { value = JSON.parse(readFileSync(file, "utf8")); } catch { throw new Error(`OpenCode config is not valid JSON: ${file}`); }
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("OpenCode config must be an object");
  if (value.plugin !== undefined && (!Array.isArray(value.plugin) || value.plugin.some((entry) => !text(entry)))) throw new Error("OpenCode config plugin must be an array of strings");
  return value;
}

function atomicWrite(file, body) {
  mkdirSync(path.dirname(file), { recursive: true });
  const temporary = path.join(path.dirname(file), `.${path.basename(file)}.${process.pid}.${Date.now()}`);
  let descriptor;
  try {
    descriptor = openSync(temporary, "wx", 0o600);
    writeFileSync(descriptor, body);
    fsyncSync(descriptor);
    closeSync(descriptor);
    descriptor = undefined;
    renameSync(temporary, file);
  } catch (error) {
    if (descriptor !== undefined) closeSync(descriptor);
    try { unlinkSync(temporary); } catch (cleanup) { if (cleanup.code !== "ENOENT") throw cleanup; }
    throw error;
  }
}

export function configure(options = {}) {
  const file = options.file || configPath(options.environment);
  const remove = options.remove === true;
  const specifier = options.specifier || defaultSpecifier;
  if (!remove && (!text(specifier) || !(/^file:/u.test(specifier) || /^https?:\/\//u.test(specifier) || /^@sessionbus\/opencode@[^\s]+$/u.test(specifier)))) throw new Error("OpenCode plugin specifier is invalid");
  if (remove && !existsSync(file)) return false;
  const config = decode(file);
  const previous = config.plugin || [];
  const plugin = previous.filter((entry) => entry !== specifier && !owned(entry));
  if (!remove) plugin.push(specifier);
  const next = { ...config };
  if (plugin.length) next.plugin = plugin;
  else delete next.plugin;
  const body = `${JSON.stringify(next, null, 2)}\n`;
  if (existsSync(file) && readFileSync(file, "utf8") === body) return false;
  (options.write || atomicWrite)(file, body);
  return true;
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
