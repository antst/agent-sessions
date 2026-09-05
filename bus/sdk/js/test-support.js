"use strict";

const { Duplex } = require("node:stream");

class MemorySocket extends Duplex {
  _read() {}
  _write(chunk, _encoding, callback) { if (this.failNext) { this.failNext = false; callback(new Error("write failed")); return; } if (!this.peer.destroyed) this.peer.push(Buffer.from(chunk)); callback(); }
  _destroy(error, callback) { if (!this.peer.destroyed) this.peer.destroy(); callback(error); }
}

function pair() { const one = new MemorySocket(), two = new MemorySocket(); one.peer = two; two.peer = one; return [one, two]; }
function deferred() { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }

module.exports = { deferred, MemorySocket, pair };
