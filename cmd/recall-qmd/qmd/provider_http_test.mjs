import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {once} from 'node:events';
import {createHTTPInference, verifyPin} from './provider.mjs';

async function server(t, handler) {
  const http = createServer(async (req, res) => {
    let raw = ''; for await (const part of req) raw += part;
    const body = JSON.parse(raw);
    const response = handler(req, body);
    res.writeHead(response.status || 200, {'content-type': 'application/json'});
    res.end(JSON.stringify(response.body ?? response));
  });
  http.listen(0, '127.0.0.1'); await once(http, 'listening');
  t.after(() => new Promise(resolve => http.close(resolve)));
  return `http://127.0.0.1:${http.address().port}`;
}
function config(url, extra = {}) { return {url, tokenizer_url:url, model:'immutable-gguf-content-id', dimensions:3, ...extra}; }

test('generic HTTP embedding/tokenizer contract and batch ordering', async t => {
  const seen = [];
  const url = await server(t, (req, body) => {
    seen.push([req.url,body]);
    assert.equal(body.model,'immutable-gguf-content-id');
    assert.equal(body.metadata,undefined);
    if(req.url==='/v1/tokenize') return {model:body.model,tokens:[1,2]};
    if(req.url==='/v1/detokenize') return {model:body.model,content:'decoded'};
    assert.equal(req.url,'/v1/embeddings');
    assert.equal(body.encoding_format,'float');
    assert.deepEqual(body.input,['task: search result | query: test','title: One | text: Evidence']);
    return {model:body.model,data:[{index:1,embedding:[0,1,0]},{index:0,embedding:[1,0,0]}]};
  });
  const p = createHTTPInference(config(url));
  const result = await p.embedBatch(['task: search result | query: test','title: One | text: Evidence']);
  assert.deepEqual(result.map(x=>x.embedding),[[1,0,0],[0,1,0]]);
  assert.deepEqual(await p.tokenize('text'),[1,2]);
  assert.equal(await p.detokenize([1,2]),'decoded');
  await assert.rejects(p.expandQuery('query'),/unsupported/);
  await assert.rejects(p.rerank('query',[]),/unsupported/);
  assert.equal(seen.length,3);
});

test('Rook metadata is explicit and credentials remain headers', async t => {
  const previous = process.env.RECALL_HTTP_TEST_KEY;
  process.env.RECALL_HTTP_TEST_KEY = 'test-credential';
  t.after(()=> { if(previous===undefined) delete process.env.RECALL_HTTP_TEST_KEY; else process.env.RECALL_HTTP_TEST_KEY=previous; });
  const url = await server(t,(req,body)=> {
    assert.equal(req.headers.authorization,'Bearer test-credential');
    assert.deepEqual(body.metadata,{rook:{localOnly:true,priority:'background',maxWaitSeconds:0}});
    assert.equal(JSON.stringify(body).includes('test-credential'),false);
    return {model:body.model,data:[{index:0,embedding:[1,0,0]}]};
  });
  await createHTTPInference(config(url,{rook:true,api_key_env:'RECALL_HTTP_TEST_KEY'})).embed('formatted');
});

test('provider failures and malformed vectors never become an empty success', async t => {
  const good = {model:'immutable-gguf-content-id',data:[{index:0,embedding:[1,0,0]}]};
  const cases = [
    {status:503,body:{error:'secret input must not escape'}},
    {...good,model:'wrong-model'}, {...good,data:[]},
    {...good,data:[{index:1,embedding:[1,0,0]}]},
    {...good,data:[{index:0,embedding:[1,0]}]},
    {...good,data:[{index:0,embedding:[0,0,0]}]},
    {...good,data:[{index:0,embedding:[1,'text',0]}]},
    {...good,data:[{index:0,embedding:[null,0,0]}]},
  ];
  for (const response of cases) {
    const url = await server(t,()=>response);
    await assert.rejects(createHTTPInference(config(url)).embed('private corpus text'), error=> {
      assert.equal(error.message.includes('secret'),false);
      assert.equal(error.message.includes('private corpus'),false);
      return /HTTP inference/.test(error.message);
    });
  }
  await assert.rejects(createHTTPInference(config('http://127.0.0.1:1')).embed('text'),/unreachable/);
});

test('index pin refuses adoption and model changes even during refresh',()=> {
  verifyPin(undefined,'space-a',0,true);
  verifyPin('space-a','space-a',5,false);
  for (const [previous,wanted,vectors,maintenance] of [[undefined,'space-a',1,true],[undefined,'space-a',0,false],['space-a','space-b',0,true],['space-a','space-b',20,false]]) {
    assert.throws(()=>verifyPin(previous,wanted,vectors,maintenance),/contract mismatch/);
  }
});


test('OpenAI v1 base URL is accepted without doubling the version path', async t => {
  const root = await server(t,(req,body)=> {
    assert.equal(req.url,'/v1/embeddings');
    return {model:body.model,data:[{index:0,embedding:[1,0,0]}]};
  });
  await createHTTPInference(config(root+'/v1')).embed('formatted');
});
