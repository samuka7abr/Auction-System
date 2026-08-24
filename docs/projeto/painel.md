# Painel ao vivo

[← índice](../README.md)

*Etapa 5b — **abandonada**. Este documento fica como registro do que foi desenhado e não construído: não existe `web/`, e os números do mock abaixo são ilustrativos, nunca medidos. Os números reais estão em [benchmark.md](benchmark.md#resultados).*

Não é uma interface de leilão. É um **painel de instrumentos**: três colunas lado a lado, uma por estratégia, consumindo os três `auctiond` simultaneamente enquanto o k6 martela os três com carga idêntica.

```
┌─ OTIMISTA ────────┐ ┌─ PESSIMISTA ──────┐ ┌─ SINGLE-WRITER ───┐
│  R$ 4.180         │ │  R$ 5.920         │ │  R$ 9.740         │
│  ▁▂▂▃▃▃▃▃▃        │ │  ▁▂▃▄▅▅▆▆▆        │ │  ▁▃▅▆▇█████       │
│  aceitos/s   142  │ │  aceitos/s   380  │ │  aceitos/s  1.910 │
│  conflitos/s 2.4k │ │  espera lock 41ms │ │  lote          87 │
│  p95       890ms  │ │  p95        210ms │ │  p95         12ms │
└───────────────────┘ └───────────────────┘ └───────────────────┘
```

O que se vê em dez segundos de GIF: a coluna otimista travando e enchendo de conflito enquanto a single-writer continua fluindo. É a tese do projeto inteira, sem ler uma linha do README.

Os três `p95` são comparáveis porque medem a mesma coisa nas três colunas: latência até **durável**. A coluna do shard mostra o tamanho do lote no lugar de conflitos, porque é de onde vem o ganho dela.

---

## Stack

**React com TypeScript, via Vite.** Sem router, sem biblioteca de estado, sem framework de UI. Três dependências de runtime no total.

TypeScript aqui não é preferência, é requisito funcional: o painel consome payloads de WebSocket de três serviços em paralelo, e um campo renomeado no Go precisa quebrar o build do front, não aparecer como `undefined` no meio de um GIF de demonstração.

---

## Contrato tipado

Um único arquivo de tipos espelha os payloads do `auctiond`, gerado a partir das structs Go para não sair de sincronia na mão.

```bash
# make types
go run github.com/gzuidhof/tygo@latest generate
```

```ts
// web/src/types.gen.ts (gerado)
export type Strategy = "optimistic" | "pessimistic" | "shard";

export interface AuctionTick {
  auctionId: string;
  strategy: Strategy;
  highestBid: number;
  seq: number;
  acceptedPerSec: number;
  conflictsPerSec: number;
  inboxDepth: number;
  batchSize: number;
  p95ConfirmMs: number;
  ts: string;
}
```

---

## Um hook, e só

Todo o estado do painel cabe em um hook por estratégia. Nada mais que isso é necessário.

```ts
export function useAuctionFeed(strategy: Strategy, url: string) {
  const [tick, setTick] = useState<AuctionTick | null>(null);
  const [history, setHistory] = useState<number[]>([]);
  const [status, setStatus] = useState<"connecting" | "live" | "down">("connecting");

  useEffect(() => {
    const ws = new WebSocket(url);
    ws.onopen = () => setStatus("live");
    ws.onclose = () => setStatus("down");
    ws.onmessage = (e) => {
      const t: AuctionTick = JSON.parse(e.data);
      setTick(t);
      setHistory((h) => [...h.slice(-59), t.highestBid]);
    };
    return () => ws.close();
  }, [url]);

  return { tick, history, status };
}
```

Três componentes: `<App>` monta as colunas, `<StrategyPanel>` renderiza uma delas, `<Sparkline>` desenha o histórico em SVG puro. O `status` importa: quando o `auctiond` cai durante o `make chaos`, a coluna fica vermelha e volta sozinha, o que é a evidência visual da reassunção de shard.

---

## O detalhe que importa

O servidor emite um tick agregado a cada 100ms por leilão, **não um evento por lance**. Sob 1000 VUs isso seria dezenas de milhares de mensagens por segundo por cliente, e o gargalo passaria a ser o navegador em vez do sistema testado.

Essa decisão é o motivo de o painel valer como teste e não só como vitrine: cliente lento não pode virar backpressure no hub de WebSocket. Se o `shard_inbox_depth` subir porque uma aba travou, o acoplamento está errado e o bug é real.

---

## Fora de escopo no front

Sem login, sem catálogo, sem formulário de lance, sem perfil, sem histórico. Quem dá lance aqui é o k6. Interface de e-commerce faria o projeto ser lido como CRUD com front bonito, que é exatamente o oposto do posicionamento.
