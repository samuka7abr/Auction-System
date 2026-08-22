# Etapa 2 — Spec 03: Duplicatas sob carga e invariantes de idempotência

[← índice](../../README.md) · [decisões da etapa 1](../../decisoes/etapa-1.md) · [decisões da etapa 2](../../decisoes/etapa-2.md) · [spec 01](01-spec-engine-pessimista.md) · [spec 02](02-spec-idempotencia.md)

## Contexto

A spec 01 entregou a segunda engine. A spec 02 pôs o middleware de idempotência acima das duas engines, fez a chave chegar ao banco, publicou as métricas e ensinou o k6 a manter uma chave por lance lógico. O mecanismo está implementado e coberto contra Redis real, mas a promessa pública da etapa 2 ainda não foi demonstrada sob carga.

O próprio código marca os três pontos deixados para esta spec:

- `bench/bid-storm.js` envia a chave, mas diz que a injeção de duplicatas é da spec 03
- `cmd/checker/client.go` ainda trata `db > cliente` como aviso e diz que a etapa 2 remove essa tolerância
- `cmd/checker/invariants.go` termina I1..I4 com o lugar de I7 reservado para a idempotência

Isso significa que a próxima entrega não é ainda a engine single-writer da etapa 3. Antes dela, a etapa 2 precisa cumprir a última linha do próprio roadmap: reenviar duplicatas de propósito, provar que nenhuma virou uma segunda linha e transformar o replay em evidência independente de durabilidade.

Sustentam esta spec as decisões **4** (a verdade de `201` vem do cliente), **5** (apostador agressivo), **9** (`409` e `422` são equivalentes para a retentativa), **13** (estado inicial idêntico), **18** (dez tentativas ou dois segundos), **31** (a chave nomeia o lance lógico), **33** (`425` imediato), **34** (replay byte a byte), **35** (tentativas contadas no claim), **38** (métricas de cliente e servidor convivem), **39** (a chave chega ao banco) e **40** (I5 aperta nesta spec), mais as decisões **42** a **47**, tomadas aqui.

## Objetivo

Fechar a etapa 2 demonstrando a idempotência sob a mesma carga que mede as engines e fazendo o checker reprovar qualquer ambiguidade entre o que o cliente confirmou e o que o banco gravou.

O sistema deve:

- Selecionar deterministicamente 10% dos **lances lógicos** para receber uma requisição duplicada com a mesma chave
- Exercitar duplicata concorrente e replay posterior ao aceite sem criar um novo eixo na matriz
- Tratar `425 idempotency_in_flight` como resposta esperada e contar `425` e replay separadamente
- Contar `accepted` uma vez por lance lógico, mesmo quando mais de um `201` chega para a mesma chave
- Retentar erro de transporte com a mesma chave, permitindo que um `201` perdido reapareça como replay
- Fazer I5 exigir igualdade exata entre aceites lógicos e linhas duráveis
- Acrescentar I7: toda linha da célula tem chave e nenhuma chave se repete
- Entregar tudo sem alterar uma linha de `internal/`, `cmd/auctiond`, schema, compose ou API

## Fora de Escopo

- Engine single-writer, shards, canais, journal e commit em lote — etapa 3
- Redis Streams, `closerd` e caos — etapa 4
- Matriz de 36 células, sweep de pool e dashboards — etapa 5
- Nova flag ou variável para ligar idempotência. O benchmark padrão passa a demonstrá-la; a célula sem header continua possível executando o cliente manualmente
- Nova semântica no middleware, novo código HTTP ou mudança de TTL. A spec 02 já fechou esse contrato
- Impressão digital do corpo. A chave continua nomeando o lance lógico e atravessando corpos re-mirados
- Retentativa interna no servidor. Toda recuperação de transporte continua sendo responsabilidade do cliente
- Consulta ao Prometheus pelo checker. Métricas do servidor aparecem apenas nos checkpoints diagnósticos

## Fluxo

```text
um lance lógico: key = UUID(__VU, __ITER, nonce)
  ├── plano normal (90%)
  │     └── POST por tentativa
  │
  └── plano duplicado (10%, determinístico)
        ├── metade concorrente
        │     └── primeira tentativa: http.batch([request, mesma request])
        │           ├── 201 fresco + 425
        │           ├── 201 fresco + 201 replay
        │           └── rejeição + 425/rejeição, depois o laço re-mira
        │
        └── metade posterior
              └── quando o lance recebe o primeiro 201:
                    POST do mesmo body com a mesma key
                      └── 201 + X-Idempotency-Replayed: true

redução das respostas de uma tentativa
  ├── primeiro 201 observado para a key
  │     ├── bids_accepted += 1              (lance lógico, uma vez)
  │     ├── X-Idempotency-Replayed=true     → bids_replayed += 1
  │     └── absorve estado e encerra o lance
  ├── todo 201 adicional com header         → bids_replayed += 1, nunca accepted
  ├── 425                                   → bids_in_flight += 1, sem estado
  ├── status 0                              → bids_error + transport_retries; mesma key retenta
  ├── 409/422                               → absorve o estado mais novo e re-mira
  └── demais                                → erro terminal

depois da célula
  └── cmd/checker
        ├── I1..I4  invariantes da história
        ├── I5      count(bids) == client.accepted
        │           max(seq) == client.maxSeqSeen
        ├── I6      célula e injeção válidas
        └── I7      count(*) == count(key) == count(distinct key)
```

## Decisoes Tecnicas

### A taxa é por lance lógico, não por requisição bruta

“Duplicar 10% das requisições” parece preciso e é metodologicamente instável. O otimista produz mais requisições por lance aceito justamente por causa dos retries; selecionar 10% depois dessa amplificação faria a quantidade de duplicatas depender da engine que está sendo medida.

A unidade estável é o lance lógico, a mesma que a chave nomeia. Um seletor puro de `(__VU, __ITER)` põe exatamente uma de cada dez iterações na coorte de duplicação, antes de qualquer desfecho. A decisão não usa `Math.random()` dentro do laço e não ramifica por estratégia. O plano da coorte alterna entre concorrente e posterior, de modo que as duas formas apareçam sem virar eixo da matriz (decisão 42).

O resumo grava tanto a coorte selecionada quanto as requisições extras efetivamente enviadas. Um lance escolhido para replay posterior que se esgota sem `201` não tem resposta terminal para reencenar e, portanto, não envia a extra; essa diferença fica visível em vez de ser preenchida por uma requisição que mudaria o resultado depois do prazo.

### A duplicata é carga extra; não é uma tentativa lógica

`bids_attempts` e `client_attempts_per_accept` continuam contando as tentativas do apostador. A requisição extra entra em `duplicates_injected`, não nesses dois números.

Isso preserva o significado já publicado: a amplificação do cliente é quantas vezes ele precisou re-mirar para conseguir um lance, enquanto a injeção é a perturbação controlada que testa o middleware. Se a duplicata concorrente escapar da janela de `busy` depois de uma rejeição e chegar à engine, `bid_attempts_per_accept` do **servidor** a contará — e essa é a verdade do servidor, não algo a esconder (decisão 43).

### `accepted` conta fatos lógicos, não respostas HTTP

Depois desta spec, pode haver dois `201` para uma chave: o fresco e o replay. Somá-los em `bids_accepted` faria o checker acusar lance perdido exatamente quando a idempotência funcionou.

O contador mantém o nome, mas seu contrato fica explícito: incrementa uma vez quando o lance lógico observa seu primeiro `201`, com ou sem o header de replay. Todo `201` com `X-Idempotency-Replayed: true` também incrementa `bids_replayed`; nunca incrementa `accepted` pela segunda vez. Assim:

```text
client.accepted == bid_outcomes_total{outcome="accepted"} == count(bids)
client.replayed == respostas 201 servidas pelo Redis, inclusive recuperação de transporte
```

Essa igualdade vale quando as três fontes são recortadas à mesma janela medida. O contador Prometheus é cumulativo desde o boot e o warmup é apagado apenas do banco; portanto o checkpoint usa cliente e banco como prova e deixa a série do servidor como diagnóstico. O primeiro número continua comparável entre as três engines. O segundo mede o middleware e não entra no eixo de throughput (decisão 44).

### Uma tentativa pode produzir duas respostas, e a redução tem precedência

Na metade concorrente, `http.batch` devolve as duas respostas sem dizer qual foi “a original”. O cliente não deve inventar essa identidade. Ele reduz o par por semântica:

1. Qualquer `201` resolve o lance; se houver dois, conta um aceite lógico e classifica separadamente o que tiver header de replay
2. `425` só incrementa `inFlight`; ele não tem estado e nunca substitui uma resposta com estado
3. Entre `409` e `422`, absorve o corpo com maior `currentVersion`
4. Erro de transporte é retentável se nenhuma outra resposta do par já resolveu o lance
5. Resposta inesperada continua terminal e alimenta `bids_error`

Essa ordem impede o `425` concorrente de apagar o `201` da mesma tentativa e impede uma rejeição mais velha de fazer o cache andar para trás (decisão 43).

### Erro de transporte mantém a chave e a ambiguidade fica aberta

No k6, erro de transporte aparece como `status == 0`. Ele passa a consumir uma tentativa normal, incrementar `bids_error` e `transport_retries`, aplicar a política de backoff e continuar dentro dos limites já publicados de dez tentativas e dois segundos — sempre com a mesma chave e o mesmo estado lógico.

Se o servidor tiver comitado antes de a resposta desaparecer, a próxima tentativa encontra `done` e recebe replay. Se a requisição nunca chegou, ela encontra passagem e executa normalmente. O cliente não tenta adivinhar qual aconteceu (decisão 45).

`503` continua erro real e terminal nesta spec. Transformá-lo em retry mudaria a carga durante falha de infraestrutura e responderia a uma pergunta de resiliência que pertence à etapa 4.

### I5 deixa de aceitar banco à frente

Na etapa 1, `db > client` era aviso legítimo: um `201` podia ser durável e sua resposta sumir. A spec 02 entregou a peça que remove a ambiguidade; esta spec passa a usá-la.

I5 exige:

```text
db.count == client.accepted
db.maxSeq == client.maxSeqSeen
```

Qualquer desigualdade é `FAIL`. Banco atrás continua significando write confirmado perdido. Banco à frente passa a significar que o cliente não reconciliou uma resposta ambígua, que contou aceite errado ou que uma chave foi executada sem ser observada. Todos os três tornam a célula imprestável (decisão 46).

### I7 prova presença e unicidade na célula medida

O índice parcial já torna duas chaves não nulas iguais impossíveis sob o schema esperado. Isso não torna I7 inútil: a engine pode esquecer de preencher a coluna e escrever `NULL`, escapando exatamente do índice que deveria protegê-la.

I7 compara três números:

```sql
SELECT count(*), count(idempotency_key), count(DISTINCT idempotency_key)
FROM bids;
```

Os três precisam ser iguais. A igualdade prova que toda linha da célula veio com chave e que nenhuma chave nomeou duas linhas. O teste de violação planta `NULL` numa linha existente; não derruba o índice para fabricar um estado que o schema de produção não permite (decisão 47).

### Observabilidade do cliente

Entram cinco contadores no k6 e cinco campos escalares em `client.json`:

| Métrica k6 | Campo | Significado |
| --- | --- | --- |
| `bids_replayed` | `replayed` | respostas `201` com `X-Idempotency-Replayed: true` |
| `bids_in_flight` | `inFlight` | respostas `425` a duplicata concorrente |
| `duplicates_selected` | `duplicatesSelected` | lances lógicos colocados na coorte de 10% |
| `duplicates_injected` | `duplicatesInjected` | requisições extras realmente enviadas |
| `transport_retries` | `transportRetries` | status 0 que mantiveram a chave e retentaram |

`accepted`, `attempts`, `clientAttemptsPerAccept` e todos os campos existentes permanecem presentes. Nenhum é renomeado.

I6 falha se a corrida medida não selecionar ou não injetar duplicata alguma, ou se não observar replay algum. `inFlight == 0` é aviso, não falha: a carga foi concorrente, mas numa engine muito rápida a segunda requisição pode encontrar `done` em vez de `busy`; o comportamento continua correto e aparece como replay. `duplicatesInjected > duplicatesSelected` é sempre falha: cada lance selecionado pode enviar no máximo uma requisição extra.

## Requisitos Funcionais

### RF01 - Seleção determinística da coorte

`bench/bid-storm.js` seleciona uma em cada dez iterações por uma função pura de `__VU` e `__ITER`. O resultado independe da estratégia, da política de retry, dos status recebidos e do número de tentativas.

A coorte alterna deterministicamente os modos `concurrent` e `replay`. Não entra `DUPLICATE_RATE`, `DUPLICATE_MODE` nem outra variável de ambiente: duplicatas são uma condição fixa da etapa 2 e não um novo eixo experimental.

### RF02 - Duplicata concorrente

Na primeira tentativa de um lance selecionado como `concurrent`, o cliente envia duas requisições idênticas por `http.batch`, com mesmo URL, body, headers, usuário e chave.

`duplicatesInjected` incrementa uma vez. As duas respostas passam pelo redutor descrito nas decisões técnicas. O laço continua normal se nenhuma delas aceitar, sempre com a mesma chave e absorvendo o estado mais novo disponível.

### RF03 - Replay posterior

Quando um lance selecionado como `replay` recebe seu primeiro `201`, o cliente reenvia uma vez o mesmo URL, body, usuário e chave que produziram o aceite.

A resposta esperada é `201` com `X-Idempotency-Replayed: true` e corpo byte a byte igual. Ela incrementa `duplicatesInjected` e `replayed`, mas não `accepted`, `attempts`, `seq_seen`, `client_attempts_per_accept` nem `bid_confirm_latency`.

Resposta diferente é contabilizada em `bids_error` e deixa a célula reprovável; nunca é silenciosamente tratada como outro lance.

### RF04 - Status e contadores

`http.expectedStatuses` passa a incluir `425`, além de `201`, `409`, `410` e `422`. O corpo de `425` não é passado a `absorb`, porque por contrato não carrega estado.

O primeiro `201` observado resolve o lance lógico e incrementa `accepted` uma vez. Todo `201` com o header de replay incrementa `replayed`. Todo `425` incrementa `inFlight`.

`status == 0` incrementa `bids_error` e `transportRetries` e retenta com a mesma chave enquanto `MAX_RETRIES` e `BID_DEADLINE` permitirem. Os demais status inesperados continuam terminais.

### RF05 - Contrato de `client.json`

`handleSummary` acrescenta `replayed`, `inFlight`, `duplicatesSelected`, `duplicatesInjected` e `transportRetries` como números obrigatórios. Contador sem amostra vira zero pela função já usada para os demais counters; campo presente com formato inválido continua abortando o resumo.

`accepted` passa a ser documentado e implementado como quantidade de lances lógicos que observaram ao menos um `201`, sem dupla contagem de replay.

`cmd/checker/client.go` lê os cinco campos como ponteiros e os inclui em `required()`. Artefato antigo ou incompleto passa a sair com código 2, não com zeros inventados.

### RF06 - I5 exato e I6 consciente da injeção

`checkDurability` reprova `db.Bids != client.Accepted` em qualquer direção e reprova `db.MaxSeq != client.MaxSeqSeen`.

`checkCellValidity` mantém as regras existentes e acrescenta:

- `duplicatesSelected == 0` ou `duplicatesInjected == 0` → `FAIL`
- `replayed == 0` → `FAIL`: a metade posterior não demonstrou replay
- `inFlight == 0` → `WARN`: a duplicata concorrente pode ter encontrado `done`
- qualquer contador negativo ou `duplicatesInjected > duplicatesSelected` → `FAIL`

O detalhe da linha I6 publica os quatro números, mesmo quando todos estão válidos.

### RF07 - I7

`cmd/checker/invariants.go` acrescenta I7, “chaves idempotentes presentes e únicas”, usando os três counts da decisão 47 e imprimindo-os no detalhe tanto em verde quanto em falha.

O relatório continua na ordem I1 a I7. O checker executa todos os invariantes mesmo quando um falha e preserva os códigos `0` verde, `1` violação e `2` não verificável.

### RF08 - O servidor não muda

O diff de implementação não pode conter arquivos em:

```text
internal/
cmd/auctiond/
cmd/seed/
migrations/
docker-compose.yaml
go.mod
go.sum
```

O middleware, as engines, o contrato HTTP, o schema e as métricas do servidor já entregam tudo de que esta spec precisa. Se o harness parecer exigir mudança neles, **pare e reporte**: é uma falha de contrato da spec 02, não escopo adicional desta.

## Requisitos Nao Funcionais

- Nenhuma dependência nova, Go ou JavaScript
- O mesmo `bench/bid-storm.js` roda nas três estratégias sem `if strategy`
- Uma requisição extra no máximo por lance selecionado; nenhuma duplicação recursiva
- O seletor não usa relógio, resposta HTTP nem aleatoriedade por tentativa
- O replay posterior reutiliza os bytes do body que aceitaram, não um body recalculado a partir do cache já atualizado
- O redutor nunca deixa o cache de um leilão andar para uma versão menor
- O checker continua sem importar cliente Prometheus nem fazer chamada HTTP
- `go test ./...`, `go vet ./...` e `gofmt -l .` limpos; `k6 archive bench/bid-storm.js` sem erro

## Budget do PR

Até 7 arquivos e aproximadamente 450 linhas de código próprio.

O servidor está pronto. Esta entrega é um corte estreito sobre o gerador e o verificador; ultrapassar o budget indica que a idempotência está sendo redesenhada em vez de demonstrada. Nesse caso, pare e reporte.

## Claude Code

- Modelo: `claude-opus-5`
- Esforco: alto
- Referencia permitida: `docs/projeto/provas.md`, `docs/projeto/benchmark.md`, `docs/decisoes/etapa-1.md`, `docs/decisoes/etapa-2.md`, `docs/specs/etapa-1/03-spec-carga-e-checker.md`, `docs/specs/etapa-2/02-spec-idempotencia.md`, `docs/specs/etapa-2/03-spec-duplicatas-e-invariantes.md`

Prompt:

```text
Implemente docs/specs/etapa-2/03-spec-duplicatas-e-invariantes.md no repositorio bid-storm.

Leia antes de comecar:
  docs/specs/etapa-2/03-spec-duplicatas-e-invariantes.md  (a spec — autoridade)
  docs/decisoes/etapa-2.md                                (decisoes 31..47)
  docs/specs/etapa-2/02-spec-idempotencia.md              (o mecanismo pronto)
  bench/bid-storm.js                                      (o cliente a alterar)
  cmd/checker/client.go                                   (I5/I6 e o contrato)
  cmd/checker/invariants.go                               (I1..I4 e o lugar de I7)

Escopo: apenas RF01..RF08. Injete duplicatas no k6, conte replay/425,
retente status 0 com a mesma chave, aperte I5 e acrescente I7.

Regras:
- NAO altere internal/, cmd/auctiond/, cmd/seed/, migrations/,
  docker-compose.yaml, go.mod ou go.sum. Se algum deles parecer necessario,
  pare e reporte: o contrato da spec 02 esta errado.
- O checker NAO consulta Prometheus.
- accepted conta lances logicos, nunca respostas 201 brutas.
- 425 e esperado, nao carrega estado e nunca vira conflict.
- A requisicao duplicada nao entra em bids_attempts nem em
  client_attempts_per_accept.
- O seletor de 10% nao ramifica por estrategia e nao depende do resultado.
- Rode C1..C5 e cole a saida real. Checkpoint sem saida nao conta.
- Se passar de 7 arquivos ou ~450 linhas, pare e reporte.
- Nao altere nada dentro de docs/.
```

## Arquivos Esperados

Editar:

```text
bench/bid-storm.js
cmd/checker/main.go
cmd/checker/client.go
cmd/checker/invariants.go
cmd/checker/client_test.go
cmd/checker/invariants_test.go
```

Nenhum arquivo novo é necessário. `cmd/checker/main.go` só pode mudar para manter I1..I7 na ordem depois de I7 nascer no conjunto SQL.

## Testes

Editar:

```text
cmd/checker/client_test.go
  - db == client continua verde
  - db < client falha
  - db > client agora falha, nunca avisa
  - watermark diferente em qualquer direção falha
  - os cinco campos novos são obrigatórios
  - injeção ausente e replay ausente falham; in_flight ausente avisa

cmd/checker/invariants_test.go
  - célula coerente escreve uma chave distinta em toda linha e I7 passa
  - uma chave NULL plantada faz apenas I7 falhar
```

O k6 continua sem teste unitário fora do runtime próprio. Sua sintaxe é validada por `k6 archive`; a redução de respostas e os dois modos são validados pelos checkpoints ponta a ponta.

## Checkpoints Mensuraveis

### C1 - Testes e fronteira do diff

```bash
go test ./cmd/checker/... -race -count=1 -v
go test ./... -race -count=1
gofmt -l . && go vet ./...
docker compose run --rm --no-deps k6 archive /bench/bid-storm.js -O /tmp/bid-storm.tar
git diff --name-only -- internal cmd/auctiond cmd/seed migrations docker-compose.yaml go.mod go.sum
```

Aceite:

- Todos os testes passam, inclusive `db > client` como `FAIL` e I7 com violação plantada
- `k6 archive` termina com código 0
- `gofmt -l` e `go vet` não produzem saída
- O último comando não lista arquivo algum

### C2 - Uma célula demonstra as duas formas de duplicata

```bash
make up && sleep 15
make run STRATEGY=optimistic && sleep 10
make bench RUN=e2c3-otim STRATEGY=optimistic AUCTIONS=1 POLICY=immediate SCENARIO=smoke

jq '{accepted,replayed,inFlight,duplicatesSelected,duplicatesInjected,transportRetries}' \
  bench/results/e2c3-otim/client.json
cat bench/results/e2c3-otim/checker.txt
curl -s localhost:8080/metrics | grep idempotency_hits_total
```

Aceite:

- `accepted`, `duplicatesSelected`, `duplicatesInjected` e `replayed` são maiores que zero
- `inFlight` é maior que zero nesta célula de contenção; se for zero, repita uma vez antes de investigar a concorrência do `http.batch`
- `idempotency_hits_total` tem atividade em `replayed` e `in_flight`
- I1 a I7 saem verdes e o checker termina com código 0

### C3 - Cliente e banco contam o mesmo fato; servidor fica diagnóstico

```bash
jq -r '.accepted' bench/results/e2c3-otim/client.json
curl -s localhost:8080/metrics | grep '^bid_outcomes_total{.*outcome="accepted"'
docker compose exec -T postgres psql -U auction -d auction -tAc \
  'SELECT count(*), count(idempotency_key), count(DISTINCT idempotency_key), max(seq) FROM bids;'
jq -r '.maxSeqSeen' bench/results/e2c3-otim/client.json
```

Aceite:

- `client.accepted` e `count(*)` são iguais
- `count(*)`, `count(idempotency_key)` e `count(DISTINCT idempotency_key)` são iguais
- `max(seq)` é igual a `maxSeqSeen` com um leilão
- `replayed > 0` não aumenta nenhum dos três números de aceite
- A série do servidor existe e cresce, mas seu valor absoluto **não** é comparado: ela inclui o warmup e qualquer célula anterior desde o boot, enquanto o banco foi resetado. O checker continua usando apenas a verdade independente do cliente

### C4 - O checker reprova as duas garantias novas

```bash
cp bench/results/e2c3-otim/client.json /tmp/e2c3-client.json
jq '.accepted -= 1' /tmp/e2c3-client.json > bench/results/e2c3-otim/client.json
make check RUN=e2c3-otim; echo "I5 exit=$?"
cp /tmp/e2c3-client.json bench/results/e2c3-otim/client.json

docker compose exec -T postgres psql -U auction -d auction -c \
  'UPDATE bids SET idempotency_key = NULL WHERE id = (SELECT id FROM bids ORDER BY seq LIMIT 1);'
make check RUN=e2c3-otim; echo "I7 exit=$?"
```

Aceite:

- O primeiro check sai `1`, com I5 `FAIL` dizendo que o banco está à frente; não há mais `WARN`
- O segundo sai `1`, com I7 `FAIL` mostrando `linhas`, `com_chave` e `distintas`
- A violação plantada de I7 não exige remover a restrição única do schema

### C5 - As duas engines fecham a etapa 2

```bash
make run STRATEGY=optimistic && sleep 10
make bench RUN=e2c3-optimistic STRATEGY=optimistic AUCTIONS=1 POLICY=jitter SCENARIO=smoke
make run STRATEGY=pessimistic && sleep 10
make bench RUN=e2c3-pessimistic STRATEGY=pessimistic AUCTIONS=1 POLICY=jitter SCENARIO=smoke

for run in e2c3-optimistic e2c3-pessimistic; do
  jq -r --arg run "$run" \
    '[$run,.accepted,.replayed,.inFlight,.duplicatesInjected,.transportRetries] | @tsv' \
    "bench/results/$run/client.json"
  tail -1 "bench/results/$run/checker.txt"
done
```

Aceite:

- As duas células terminam com código 0 e I1..I7 verdes
- As duas injetam duplicatas e observam replay; nenhuma grava chave nula ou repetida
- O script não contém ramo por estratégia
- A etapa 2 fecha aqui: pessimista, idempotência, duplicatas sob carga e checker estrito entregues

## Smoke Manual

Pre-condicoes:

```text
Docker e docker compose v2, jq e make instalados
Portas 5432, 6379, 8080, 9090 e 3000 livres
.env criado a partir de .env.example
```

Passos:

```bash
make up && sleep 15
make bench RUN=manual-e2 AUCTIONS=1 SCENARIO=smoke POLICY=immediate
jq '{accepted,replayed,inFlight,duplicatesSelected,duplicatesInjected}' \
  bench/results/manual-e2/client.json
cat bench/results/manual-e2/checker.txt
docker compose exec -T postgres psql -U auction -d auction -c \
  'SELECT count(*) linhas, count(idempotency_key) com_chave,
          count(DISTINCT idempotency_key) distintas FROM bids;'
make down
```

Aceite manual:

- O resumo mostra a coorte e ao menos um replay sem somá-lo duas vezes em `accepted`
- O relatório cabe numa tela, lista I1..I7 em ordem e termina `resultado: OK`
- Os três counts do Postgres são iguais
- `make down` não deixa contêiner órfão

## Definicao De Pronto

- RF01 a RF08 implementados
- C1 a C5 executados com saída real no PR
- I5 exato em ambas as direções e I7 com teste de violação plantada
- `client.json` recusa artefato antigo sem os cinco campos novos
- Células otimista e pessimista verdes, ambas com duplicatas injetadas e replay observado
- Nenhuma linha alterada no servidor, schema, compose ou dependências
- Budget respeitado, ou desvio reportado antes de estourar
- Nenhum arquivo dentro de `docs/` alterado pelo PR de implementação
- A etapa 2 está completa e a próxima spec, depois desta, é a engine single-writer da etapa 3
