# Carga e benchmark

[← índice](../README.md)

k6, com **contenção como variável independente**. O mesmo volume total de requisições distribuído sobre 1, 10 ou 1000 leilões produz curvas completamente diferentes, e é justamente esse eixo que revela o cruzamento entre as estratégias.

---

## O modelo do apostador

Esta é a decisão que determina se o gráfico mede alguma coisa.

Se cada VU mandasse um valor fixo, depois da primeira rodada todo mundo levaria `422` e desistiria: a taxa de conflito desabaria para perto de zero e o modo otimista apareceria no gráfico como ótimo. Você teria provado o contrário da hipótese por acidente de instrumentação.

O VU é **agressivo**: ele sempre quer ganhar, e re-mira em cima do estado que a rejeição devolveu. Toda falha vira disputa genuína, que é exatamente a variável sob teste — e modela o sniping de último segundo.

```js
const POLICY   = __ENV.RETRY_POLICY || 'immediate';
const MAX_RETRIES  = 10;
const BID_DEADLINE = 2000; // ms — o que vier primeiro

const accepted  = new Counter('bids_accepted');
const conflicts = new Counter('bids_conflict');   // 409, só a otimista
const outbids   = new Counter('bids_outbid');     // 422, as três
const exhausted = new Counter('bids_exhausted');  // desistiu

function backoff(attempt) {
  if (POLICY === 'immediate') return 0;
  return Math.random() * Math.min(200, 5 * 2 ** attempt); // full jitter
}

export default function () {
  const a = pickAuction();                 // uniforme entre os N leilões
  let { amount, expectedVersion } = a.next();
  const deadline = Date.now() + BID_DEADLINE;

  for (let i = 0; i < MAX_RETRIES && Date.now() < deadline; i++) {
    const r = http.post(url(a.id), JSON.stringify({ amountCents: amount, expectedVersion }),
                        { headers: { 'X-User-Id': a.user } });

    if (r.status === 201) { accepted.add(1); maxSeq.add(r.json('seq')); return; }

    // 409 e 422 são o mesmo evento visto por mecanismos diferentes.
    // Tratar os dois igual é o que mantém a carga equivalente nas três engines.
    if (r.status === 409 || r.status === 422) {
      const b = r.json();
      (r.status === 409 ? conflicts : outbids).add(1);
      amount          = b.minNextBid;      // o servidor define o incremento
      expectedVersion = b.currentVersion;
      sleep(backoff(i) / 1000);
      continue;
    }
    return;                                 // 410/404/5xx: terminal
  }
  exhausted.add(1);
}
```

`bids_exhausted` é número de manchete do relatório, não rodapé: sob alta contenção ele **é** o colapso do otimista tornado visível. Se a taxa de exaustão passar de ~20%, `MAX_RETRIES` está mascarando o efeito e precisa subir.

---

## A política de retentativa é variável, não constante

A crítica mais forte que se pode fazer ao projeto inteiro é esta:

> *"Seu modo otimista colapsou porque você retentou sem backoff. Com jitter exponencial ele não colapsa, e sua tese cai."*

É uma crítica válida. Retry imediato sob 1000 VUs produz colapso por congestionamento, e nesse caso o gráfico estaria medindo a política de retentativa em vez do controle de concorrência.

Por isso `RETRY_POLICY=immediate|jitter` existe desde a etapa 1 — custa uma variável de ambiente — e a etapa 5 roda as duas. A crítica morre no gráfico, não no texto. E se o backoff salvar o otimista, isso **falsifica a hipótese do projeto**, o que é um resultado, não um fracasso.

Pessimista e shard ignoram a política: elas não produzem `409`.

---

## Cenários

```js
export const options = {
  scenarios: {
    ramp: {
      executor: "ramping-vus",
      stages: [
        { duration: "30s", target: 100 },
        { duration: "1m",  target: 500 },
        { duration: "30s", target: 0 },
      ],
    },
    last_second_spike: {
      executor: "constant-vus",
      vus: 1000,
      duration: "15s",
      startTime: "2m",
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"], // erro real, não 409/422
    bid_confirm_latency: ["p(95)<200", "p(99)<500"],
  },
};
```

---

## A matriz

**3 estratégias x 3 contenções x 2 cenários x 2 políticas de retry = 36 células**, com o verificador rodando após cada uma.

| Eixo | Valores |
| --- | --- |
| Estratégia | `optimistic`, `pessimistic`, `shard` |
| Contenção | 1, 10, 1000 leilões (mesmo volume total de requisições) |
| Cenário | `ramp`, `last_second_spike` |
| Política de retry | `immediate`, `jitter` |

---

## Isolamento entre células

As 36 células rodam na mesma instância de Postgres. Sem reset, a última escreveria numa tabela com milhões de linhas, índice inchado e estatísticas velhas, enquanto a primeira escreveu numa tabela vazia — e você atribuiria ao mecanismo de concorrência um efeito que é só ordem de execução. Isso invalidaria o gráfico principal do projeto.

```bash
# bench/run-matrix.sh, por célula
for cell in $CELLS; do
  psql -c 'TRUNCATE bids, auctions RESTART IDENTITY CASCADE;'
  go run ./cmd/seed -auctions=$N -ends-in=5m
  psql -c 'VACUUM ANALYZE;'

  k6 run --quiet -e WARMUP=1 bid-storm.js          # descartado: aquece cache e pool
  k6 run bid-storm.js -e AUCTIONS=$N -e RETRY_POLICY=$P \
         --summary-export=results/$RUN/summary.json

  go run ./cmd/checker -run=$RUN || exit 1          # barra a matriz
done

# célula-controle: a primeira célula é repetida por último.
# Se os dois números divergirem, houve efeito de ordem e a matriz não vale.
```

Cada run grava `results/<run>/env.json` com commit, pool, CPUs e versões — ver [arquitetura.md](arquitetura.md#ambiente-fixado).

---

## Resultados

> Medidos em 2026-08-24 no commit `6c4e90d`, árvore limpa, pool 25. São **nove células de `ramp`/`immediate`** — a fatia que desenha o gráfico principal, definida na [spec 02 da etapa 5](../specs/etapa-5/02-spec-fatia-do-grafico.md). Os números saem de `bench/results/m20260824T141228/matrix.json`, publicado por `bin/matrix`, e nenhum deles vem das métricas do processo sob teste.
>
> O pico é 500 VUs e não 1000: 1000 é o pico do `last_second_spike`, que não entrou nesta fatia.

| Estratégia | Leilões | Retry | VUs no pico | Aceitos/s | p95 confirmação | Conflitos/s | Tentativas por aceito | Exauridos | Invariantes | Avisos |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Otimista | 1 | immediate | 500 | 6.61 | 201.4 | 997.52 | 151.86 | 94.5% | OK | exaustão acima de 20%: 94.5% |
| Pessimista | 1 | immediate | 500 | 35.84 | 83.0 | 0.00 | 108.40 | 91.2% | OK | gerador saturado; exaustão acima de 20%: 91.2%; 1 aviso do checker |
| Single-writer | 1 | immediate | 500 | 46.21 | 86.0 | 0.00 | 112.27 | 91.5% | OK | gerador saturado; exaustão acima de 20%: 91.5%; 1 aviso do checker |
| Otimista | 10 | immediate | 500 | 101.65 | 173.4 | 1167.86 | 12.49 | 48% | OK | exaustão acima de 20%: 48% |
| Pessimista | 10 | immediate | 500 | 257.46 | 97.4 | 0.00 | 16.08 | 52.5% | OK | gerador saturado; exaustão acima de 20%: 52.5%; 1 aviso do checker |
| Single-writer | 10 | immediate | 500 | 319.87 | 101.0 | 0.00 | 14.63 | 49.7% | OK | gerador saturado; exaustão acima de 20%: 49.7%; 1 aviso do checker |
| Otimista | 1000 | immediate | 500 | 1397.80 | 123.4 | 1522.34 | 2.09 | 0% | OK | gerador saturado; 1 aviso do checker |
| Pessimista | 1000 | immediate | 500 | 1381.24 | 163.8 | 0.00 | 2.10 | 0% | OK | gerador saturado; 1 aviso do checker |
| Single-writer | 1000 | immediate | 500 | 1651.43 | 106.5 | 0.00 | 2.10 | 0% | OK | gerador saturado; 1 aviso do checker |

### Gráfico principal

```text
aceitos/s por contenção · ramp · immediate

                     1 leilão    10 leilões  1000 leilões
Otimista                 6.61        101.65       1397.80
Pessimista              35.84        257.46       1381.24
Single-writer           46.21        319.87       1651.43
```

**O que está estabelecido:** sob contenção máxima o otimista entrega 6.61 aceitos/s contra 35.84 do pessimista e 46.21 do single-writer — sete vezes menos, com 152 tentativas por aceito e 997 conflitos/s. O single-writer ganha nos três níveis medidos.

O achado se sustenta apesar do gerador, e não graças a ele: nas células onde o otimista perde, o k6 estava a 43% e 55% do próprio limite, enquanto pessimista e single-writer rodaram a ~105%. O perdedor mediu folgado, os vencedores mediram no teto — então 35.84 e 46.21 são pisos, e a distância real é maior, nunca menor.

### O que esta tabela não mede

1. **Vinte e sete das trinta e seis células.** Faltam os eixos `last_second_spike` (o gume do sniping) e `jitter` (retentativa com backoff). A crítica mais forte ao resultado acima — *"o otimista colapsou porque você retentou sem backoff"* — **continua sem resposta**, e as 152 tentativas por aceito da primeira linha são exatamente o que ela ataca. O harness roda essas células sem alteração nenhuma; o que faltou foi tempo de máquina.
2. **A coluna de 1 leilão tem exaustão acima de 90%** nas três engines: nove em cada dez lances lógicos desistem antes de serem aceitos. Essa coluna mede o `MAX_RETRIES` do apostador tanto quanto a engine (decisão 106).
3. **A linha de 1000 leilões é o teto do gerador.** As três engines a ~105% do limite de CPU do k6 e os três números a 15% umas das outras. O "cruzamento" aparente entre otimista e pessimista ali é de **1.2%** — menor que os **2.1%** de divergência do próprio controle, isto é, dentro do ruído que o controle existe para medir. **Não há ponto de cruzamento estabelecido por estes dados.**

### Controle

A célula 01 repetida por último (37): 6.61 vs 6.47 aceitos/s · divergência **2.1%** · OK. Abaixo dos 10% que a decisão 95 trata como ruído indistinguível de deriva, então a ordem de execução não explica a distância entre as curvas.
