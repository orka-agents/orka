import assert from 'node:assert/strict';
import test from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { createPaymentService } from './charge.mjs';

test('overlapping checkout retries create one charge', async () => {
  let charges = 0;
  const pay = createPaymentService(async amount => {
    const receipt = { id: ++charges, amount };
    await delay(15);
    return receipt;
  });
  const [first, retry] = await Promise.all([
    pay('checkout-4827', 24), pay('checkout-4827', 24),
  ]);
  assert.equal(charges, 1);
  assert.deepEqual(retry, first);
});

test('a completed checkout returns its original receipt', async () => {
  let charges = 0;
  const pay = createPaymentService(async amount => ({ id: ++charges, amount }));
  const first = await pay('checkout-4827', 24);
  assert.deepEqual(await pay('checkout-4827', 24), first);
  assert.equal(charges, 1);
});

test('different checkouts with equal amounts charge independently', async () => {
  let charges = 0;
  const pay = createPaymentService(async amount => {
    const receipt = { id: ++charges, amount };
    await delay(15);
    return receipt;
  });
  const [first, second] = await Promise.all([
    pay('checkout-4827', 24), pay('checkout-4828', 24),
  ]);
  assert.equal(charges, 2);
  assert.notEqual(first.id, second.id);
});

test('a failed charge can be tried again', async () => {
  let attempts = 0;
  const pay = createPaymentService(async amount => {
    if (++attempts === 1) throw new Error('Supplier unavailable');
    return { id: attempts, amount };
  });
  await assert.rejects(pay('checkout-4827', 24), /Supplier unavailable/);
  assert.deepEqual(await pay('checkout-4827', 24), { id: 2, amount: 24 });
  assert.equal(attempts, 2);
});

test('overlapping failed retries share one attempt, then allow another', async () => {
  let attempts = 0;
  const pay = createPaymentService(async amount => {
    const attempt = ++attempts;
    await delay(15);
    if (attempt === 1) throw new Error('Supplier unavailable');
    return { id: attempt, amount };
  });
  const results = await Promise.allSettled([
    pay('checkout-4827', 24), pay('checkout-4827', 24),
  ]);
  assert.deepEqual(results.map(result => result.status), ['rejected', 'rejected']);
  assert.equal(attempts, 1);
  assert.deepEqual(await pay('checkout-4827', 24), { id: 2, amount: 24 });
});

test('one slow checkout does not block a different checkout', async () => {
  let release;
  const blocked = new Promise(resolve => { release = resolve; });
  const pay = createPaymentService(async amount => {
    if (amount === 24) await blocked;
    return { amount };
  });
  const first = pay('checkout-4827', 24);
  try {
    const second = await Promise.race([
      pay('checkout-4828', 18), delay(250).then(() => 'blocked'),
    ]);
    assert.deepEqual(second, { amount: 18 });
  } finally {
    release();
    await first;
  }
});
