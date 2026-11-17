import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, mkdirSync, writeFileSync, symlinkSync, rmSync, readFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {spawnSync} from 'node:child_process';

function packageFixture(t) {
  const root = mkdtempSync(join(tmpdir(), 'recall-qmd-package-'));
  t.after(() => rmSync(root, {recursive: true, force: true}));
  const pkg = join(root, 'installation/node_modules/@tobilu/qmd');
  const sqlite = join(root, 'installation/node_modules/better-sqlite3');
  const link = join(root, 'configured-qmd');
  function write(path, text) {
    mkdirSync(join(path, '..'), {recursive: true});
    writeFileSync(path, text);
  }
  write(join(pkg, 'package.json'), JSON.stringify({name: '@tobilu/qmd', version: '2.8.3', type: 'module'}));
  const exports = ['createStore', 'getEmbeddingFingerprint', 'getStoreCollection', 'getStoreCollections', 'upsertStoreCollection', 'reindexCollection', 'generateEmbeddings', 'searchVec', 'getStatus', 'getCachedResult', 'setCachedResult'];
  write(join(pkg, 'dist/store.js'), exports.map(name => `export function ${name}() { throw new Error('preflight touched store'); }`).join('\n') + '\nexport const CHUNK_SIZE_TOKENS = 900, CHUNK_OVERLAP_TOKENS = 135;');
  write(join(pkg, 'dist/llm.js'), `
    export const formatQueryForEmbedding = text => 'task: search result | query: ' + text;
    export const formatDocForEmbedding = (text, title) => 'title: ' + title + ' | text: ' + text;
  `);
  write(join(pkg, 'dist/collections.js'), `export function findLocalConfigPath() { throw new Error('preflight touched config'); }`);
  write(join(pkg, 'dist/cli/formatter.js'), `export const formatSearchResults = rows => JSON.stringify(rows.map(row => ({file: row.displayPath, score: row.score, snippet: '@@ -1,2 @@'})));`);
  write(join(pkg, 'dist/db.js'), `
    export function loadSqliteVec(db) {
      if (process.env.PROBE_FAILURE === 'extension') throw new Error('fixture vector extension unavailable');
      db.vectorLoaded = true;
    }
  `);
  write(join(sqlite, 'index.js'), `
    module.exports = class Database {
      constructor(path) {
        if (path !== ':memory:') throw new Error('preflight opened a persistent database');
        if (process.env.PROBE_FAILURE === 'binding') throw new Error('fixture native binding unavailable');
      }
      prepare(sql) {
        if (!this.vectorLoaded || !sql.includes('vec_length')) throw new Error('vector extension was not exercised');
        return {get: () => ({dimensions: process.env.PROBE_FAILURE === 'dimensions' ? 0 : 3})};
      }
      close() {}
    };
  `);
  symlinkSync(pkg, link);
  return {link, sqlite};
}

function check(packagePath, failure = '') {
  return spawnSync(process.execPath, ['--input-type=module', '--eval', readFileSync(new URL('./provider.mjs', import.meta.url), 'utf8'), '--', 'check-compatibility'], {
    encoding: 'utf8', timeout: 10000,
    env: {...process.env, PROBE_FAILURE: failure, RECALL_QMD_INFERENCE: JSON.stringify({package: packagePath})},
  });
}

test('preflight resolves dependencies beside a symlinked QMD package and probes SQLite in memory', t => {
  const {link} = packageFixture(t);
  const result = check(link);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(JSON.parse(result.stdout).compatible, true);
});

test('preflight rejects missing SQLite resolution, native bindings and vector extension failures', t => {
  const {link, sqlite} = packageFixture(t);
  for (const failure of ['binding', 'extension', 'dimensions']) {
    const result = check(link, failure);
    assert.equal(result.status, 1, failure + ': ' + result.stderr);
    assert.equal(JSON.parse(result.stdout).compatible, false);
  }
  rmSync(sqlite, {recursive: true});
  const missing = check(link);
  assert.equal(missing.status, 1, missing.stderr);
  assert.equal(JSON.parse(missing.stdout).compatible, false);
});
