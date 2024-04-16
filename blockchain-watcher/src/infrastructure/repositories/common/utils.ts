import { SHA256 } from "jscrypto/SHA256";
import { Base64 } from "jscrypto/Base64";

export function divideIntoBatches<T>(set: Set<T>, batchSize = 10): Set<T>[] {
  const batches: Set<T>[] = [];
  let batch: any[] = [];

  set.forEach((item) => {
    batch.push(item);
    if (batch.length === batchSize) {
      batches.push(new Set(batch));
      batch = [];
    }
  });

  if (batch.length > 0) {
    batches.push(new Set(batch));
  }
  return batches;
}

export function hexToHash(data: string): string {
  return SHA256.hash(Base64.parse(data)).toString().toUpperCase();
}
