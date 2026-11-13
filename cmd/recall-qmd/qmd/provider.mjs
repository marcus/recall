// HTTP inference is deliberately isolated from Recall core and qmd's local CLI.
// qmd owns chunking, corpus indexing, vector storage/search, and snippets.
import { readFileSync, existsSync, mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { createHash } from 'node:crypto';
import { createRequire } from 'node:module';

const formatVersion = 'qmd-gemma-v1';
const tokenizerVersion = 'provider-tokenizer-v1';
const pinKey = 'recall_http_embedding_contract';

export function embeddingContract(config) {
  return {model: config.model, dimensions: config.dimensions, format: formatVersion, tokenizer: tokenizerVersion};
}

export function createHTTPInference(config) {
  const metadata = config.rook ? {metadata: {rook: {localOnly: true, priority: 'background', maxWaitSeconds: 0}}} : {};
  async function post(root, path, body) {
    const headers = {'Content-Type': 'application/json'};
    if (config.api_key_env) {
      const key = process.env[config.api_key_env];
      if (!key) throw new Error('HTTP inference credential environment variable is unset');
      headers.Authorization = `Bearer ${key}`;
    }
    let response;
    try {
      response = await fetch(root + path, {method: 'POST', headers, body: JSON.stringify({...body, ...metadata}), signal: AbortSignal.timeout(45000), redirect: 'error'});
    } catch {
      throw new Error('HTTP inference provider is unreachable');
    }
    // Never propagate a service's error body, URL, credentials or submitted text.
    if (!response.ok) throw new Error(`HTTP inference provider returned status ${response.status}`);
    let data;
    try { data = await response.json(); } catch { throw new Error('HTTP inference provider returned invalid JSON'); }
    if (data.model !== config.model) throw new Error('HTTP inference model identity mismatch');
    return data;
  }
  const contract = embeddingContract(config);
  const storageModel = 'recall-http-' + createHash('sha256').update(JSON.stringify(contract)).digest('hex');
  return {
    embedModelName: storageModel,
    async embed(text) { return (await this.embedBatch([text]))[0]; },
    async embedBatch(texts) {
      if (texts.length === 0) return [];
      const data = await post(config.url, '/v1/embeddings', {model: config.model, input: texts, encoding_format: 'float'});
      if (!Array.isArray(data.data) || data.data.length !== texts.length) throw new Error('HTTP inference embedding count mismatch');
      const seen = new Set();
      const results = Array(texts.length);
      for (const row of data.data) {
        if (!Number.isInteger(row.index) || row.index < 0 || row.index >= texts.length || seen.has(row.index)) throw new Error('HTTP inference embedding indices are invalid');
        seen.add(row.index);
        if (!Array.isArray(row.embedding) || row.embedding.length !== config.dimensions || row.embedding.some(x => typeof x !== 'number' || !Number.isFinite(x))) throw new Error('HTTP inference embedding dimensions or values are invalid');
        const norm = Math.hypot(...row.embedding);
        if (!Number.isFinite(norm) || Math.abs(norm - 1) > 0.01) throw new Error('HTTP inference embedding must be L2 normalized');
        results[row.index] = {embedding: row.embedding, model: storageModel};
      }
      return results;
    },
    async tokenize(content) {
      const data = await post(config.tokenizer_url, '/v1/tokenize', {model: config.model, content});
      if (!Array.isArray(data.tokens) || data.tokens.some(x => !Number.isSafeInteger(x) || x < 0)) throw new Error('HTTP inference tokenizer returned invalid tokens');
      return data.tokens;
    },
    async detokenize(tokens) {
      const data = await post(config.tokenizer_url, '/v1/detokenize', {model: config.model, tokens});
      if (typeof data.content !== 'string') throw new Error('HTTP inference detokenizer returned invalid content');
      return data.content;
    },
    async expandQuery() { throw new Error('HTTP inference query expansion is unsupported'); },
    async rerank() { throw new Error('HTTP inference reranking is unsupported'); },
    async dispose() {},
  };
}

export function verifyPin(previous, wanted, vectors, maintenance) {
  if (previous === wanted) return;
  if (previous || vectors > 0 || !maintenance) throw new Error('HTTP inference index contract mismatch or missing pin; choose a new dedicated inference_index and explicitly refresh it; existing vectors were not changed');
}

async function main(config, args) {
  const pkg = JSON.parse(readFileSync(join(config.package, 'package.json'), 'utf8'));
  // These are upstream module seams, not a fork or a second retrieval engine.
  // Refuse an unaudited API family rather than falling back to local models.
  if (pkg.name !== '@tobilu/qmd' || !/^2\.(?:[89]|[1-9][0-9])\./.test(pkg.version)) throw new Error('HTTP inference bridge requires qmd >=2.8 <3; verify compatibility before upgrading this bridge');
  if (args[0] === '--version') { console.log(`qmd ${pkg.version}`); return; }
  const mod = name => import(pathToFileURL(join(config.package, 'dist', name + '.js')).href);
  const qmd = await mod('store');
  const llm = await mod('llm');
  const collections = await mod('collections');
  const formatter = await import(pathToFileURL(join(config.package, 'dist/cli/formatter.js')).href);
  for (const name of ['createStore', 'getEmbeddingFingerprint', 'getStoreCollection', 'getStoreCollections', 'upsertStoreCollection', 'reindexCollection', 'generateEmbeddings', 'searchVec', 'getStatus']) {
    if (typeof qmd[name] !== 'function') throw new Error('HTTP inference qmd API compatibility check failed');
  }
  if (llm.formatQueryForEmbedding('probe', 'recall-http-probe') !== 'task: search result | query: probe' || llm.formatDocForEmbedding('text', 'title', 'recall-http-probe') !== 'title: title | text: text' || qmd.CHUNK_SIZE_TOKENS !== 900 || qmd.CHUNK_OVERLAP_TOKENS !== 135) throw new Error('HTTP inference qmd embedding format compatibility check failed');
  const sample = JSON.parse(formatter.formatSearchResults([{displayPath: 'notes/probe.md', docid: '123456', title: 'Probe', body: '# Probe\nEvidence text\n', score: 0.75}], 'json', {query: 'Evidence'}))[0];
  if (sample.file !== 'notes/probe.md' || sample.score !== 0.75 || !sample.snippet?.startsWith('@@ -')) throw new Error('HTTP inference qmd formatter compatibility check failed');
  if (args[0] === 'check-compatibility') {
    console.log(JSON.stringify({compatible: true, qmd_version: pkg.version, tested_version: '2.8.3', format: formatVersion, expansion: 'none'}));
    return;
  }
  const local = collections.findLocalConfigPath();
  if (local) collections.setConfigSource({configPath: local});
  const source = collections.loadConfig();
  const collectionName = args[0] === 'collection' ? args[2] : args[args.indexOf('-c') + 1];
  const definitions = source.collections || {};
  const provider = createHTTPInference(config);
  const pin = JSON.stringify({...embeddingContract(config), qmdFingerprint: qmd.getEmbeddingFingerprint(provider.embedModelName)});
  const maintenance = args[0] === 'update' || args[0] === 'embed' || (args[0] === 'collection' && args[1] === 'add');
  let store;
  try {
    // Health and queries never create a new index, register a collection, or pin it.
    if (existsSync(config.index)) {
      // Check ownership with a truly read-only handle before qmd's schema
      // initialization can migrate or repair a pre-existing index.
      const Database = createRequire(join(config.package, 'package.json'))('better-sqlite3');
      const db = new Database(config.index, {readonly: true, fileMustExist: true});
      try {
        const vectors = db.prepare('SELECT COUNT(*) AS n FROM content_vectors').get().n;
        const previous = db.prepare('SELECT value FROM store_config WHERE key = ?').get(pinKey)?.value;
        verifyPin(previous, pin, vectors, maintenance);
        const vectorTable = db.prepare("SELECT sql FROM sqlite_master WHERE name='vectors_vec'").get();
        if (vectorTable && !vectorTable.sql.includes(`float[${config.dimensions}]`)) throw new Error('HTTP inference index vector dimensions mismatch; choose a new dedicated index');
        const foreign = db.prepare('SELECT COUNT(*) AS n FROM content_vectors WHERE model != ? OR embed_fingerprint != ?').get(provider.embedModelName, qmd.getEmbeddingFingerprint(provider.embedModelName)).n;
        if (foreign) throw new Error('HTTP inference index contains incompatible embedding identities; choose a new dedicated index');
        if (!maintenance) {
          const dbModule = await mod('db');
          dbModule.loadSqliteVec(db);
          store = {db, close: () => db.close(), getStatus: model => qmd.getStatus(db, model), searchVec: (query, model, limit, collection) => qmd.searchVec(db, query, model, limit, collection, undefined, undefined, provider)};
        }
      } finally { if (!store) db.close(); }
      if (maintenance) store = qmd.createStore(config.index);
    } else if (maintenance) {
      mkdirSync(dirname(config.index), {recursive: true});
      store = qmd.createStore(config.index);
    }
    if (store) {
      store.llm = provider;
      llm.setDefaultLlamaCpp(provider); // qmd's token chunker uses its default instance.
      if (maintenance) {
        const configured = definitions[config.collection];
        const existing = qmd.getStoreCollection(store.db, config.collection);
        if (!configured || (existing && existing.path !== configured.path)) throw new Error('HTTP inference collection configuration changed its indexed path; choose a new index or correct the collection');
        store.db.prepare('INSERT INTO store_config (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value').run(pinKey, pin);
        const primary = definitions[config.collection];
        if (!primary) throw new Error('configured qmd collection is missing');
        qmd.upsertStoreCollection(store.db, config.collection, primary);
      }
    }
    function collection(name) {
      // Existing DB metadata owns the index boundary. Config is used only to
      // initialize an empty index and during explicit maintenance.
      const dbEntry = store && qmd.getStoreCollection(store.db, name);
      const value = dbEntry || (!store && name === config.collection && definitions[name]);
      if (!value) throw new Error('configured qmd collection is missing');
      return value;
    }
    if (args[0] === 'status') {
      if (!store) throw new Error('HTTP inference index is missing; explicitly refresh the dedicated index');
      const status = store ? store.getStatus(provider.embedModelName) : {totalDocuments: 0, collections: []};
      const vectors = store ? Math.max(0, status.totalDocuments - status.needsEmbedding) : 0;
      console.log(`Index: ${config.index}\nTotal: ${status.totalDocuments}\nVectors: ${vectors}\nEmbedding: ${config.model}`);
      const names = new Set([config.collection, ...status.collections.map(c => c.name)]);
      for (const name of names) {
        const c = collection(name);
        const row = status.collections.find(c => c.name === name);
        console.log(`${name} (qmd://${name}/)\nPattern: ${c.pattern || '**/*.md'}\nFiles: ${row?.documents || 0}`);
      }
    } else if (args[0] === 'collection' && args[1] === 'show') {
      if (!(store && qmd.getStoreCollection(store.db, collectionName)) && !(!store && collectionName === config.collection && definitions[collectionName])) { console.log(`Collection not found: ${collectionName}`); return; }
      const c = collection(collectionName);
      console.log(`Collection: ${collectionName}\nPath: ${c.path}\nPattern: ${c.pattern || '**/*.md'}\nInclude: yes`);
    } else if (args[0] === 'collection' && args[1] === 'add') {
      const name = args[args.indexOf('--name') + 1];
      const mask = args[args.indexOf('--mask') + 1];
      const c = qmd.getStoreCollection(store.db, name);
      if (c && c.path !== args[2]) throw new Error('derived collection indexes another directory');
      qmd.upsertStoreCollection(store.db, name, {path: args[2], pattern: mask});
      console.log('Collection registered');
    } else if (args[0] === 'update') {
      for (const c of qmd.getStoreCollections(store.db)) {
        // qmd owns scanning, chunk identities and content updates. Do not run
        // arbitrary config update hooks as a side effect of this bridge.
        await qmd.reindexCollection(store, c.path, c.pattern || '**/*.md', c.name, {ignorePatterns: c.ignore || []});
      }
      console.log('Index updated');
    } else if (args[0] === 'embed') {
      const result = await qmd.generateEmbeddings(store, {collection: collectionName, model: provider.embedModelName});
      if (result.errors) throw new Error('HTTP inference embedding refresh was incomplete');
      console.log(`Embedded: ${result.chunksEmbedded}`);
    } else if (args[0] === 'vsearch') {
      if (!store) throw new Error('HTTP inference index is missing; explicitly refresh the dedicated index');
      const query = args[args.length - 1];
      const limit = Number(args[args.indexOf('-n') + 1]);
      const rows = await store.searchVec(query, provider.embedModelName, limit, collectionName);
      const results = rows.filter(row => row.score >= 0.3).map(row => ({...row, file: row.filepath}));
      const encoded = JSON.parse(formatter.formatSearchResults(results, 'json', {query}));
      for (let i = 0; i < encoded.length; i++) {
        encoded[i].file = results[i].filepath;
        encoded[i].score = Math.round(results[i].score * 10000) / 10000;
      }
      console.log(JSON.stringify(encoded));
    } else {
      throw new Error('unsupported qmd HTTP inference operation');
    }
  } finally { if (store) store.close(); }
}

if (process.env.RECALL_QMD_INFERENCE) {
  try { await main(JSON.parse(process.env.RECALL_QMD_INFERENCE), process.argv.slice(1)); }
  catch (error) {
    // Known local diagnostics are safe; upstream errors can contain corpus text.
    const message = error instanceof Error ? error.message : '';
    console.error(message.startsWith('HTTP inference') || message.startsWith('configured qmd') || message.startsWith('unsupported qmd') ? message : 'qmd HTTP inference bridge failed; check package, index and collection configuration');
    process.exitCode = 1;
  }
}
