# Etapa 5 — Spec 01: A matriz de 36 células

[← índice](../../README.md) · [decisões da etapa 1](../../decisoes/etapa-1.md) · [decisões da etapa 2](../../decisoes/etapa-2.md) · [decisões da etapa 3](../../decisoes/etapa-3.md) · [decisões da etapa 4](../../decisoes/etapa-4.md) · [decisões da etapa 5](../../decisoes/etapa-5.md) · [etapa 4, spec 02](../etapa-4/02-spec-caos.md)

## Contexto

As etapas 1 a 4 construíram a célula e depois provaram que ela sobrevive ao pior instante. O que nenhuma delas fez foi **rodar o experimento**.

O projeto inteiro existe para responder uma pergunta — *qual estratégia de concorrência sustenta throughput e latência quando N clientes disputam o mesmo leilão, e onde exatamente cada uma quebra* — e essa resposta é um gráfico com três curvas e um ponto de cruzamento. Até aqui existem números avulsos: `c1`, `c3`, `c4-alta`, `c4-baixa`, `c5-imm`, `c5-jit` e os cinco `chaos-*`. Todos serviram para verificar mecanismo. Nenhum pode entrar no gráfico, e a razão está escrita desde a etapa 4: eles foram medidos **antes** de o `auctiond` ganhar a varredura de vencidos e antes de a máquina ganhar o `closerd` (decisão 80).

O `bench/run-cell.sh` já é o tijolo. Ele diz isso de si mesmo, em inglês, na primeira linha do arquivo:

> *"This is the brick etapa 5 repeats 36 times."*

E existem três outras frases, escritas em três arquivos diferentes, que só viram verdade neste PR:

- `cmd/checker/main.go` declara `exitViolated = 1` e `exitUnverifiable = 2` como códigos distintos porque *"both 1 and 2 stop the matrix of etapa 5"* — e nunca existiu laço nenhum lendo esses códigos
- o `Makefile` documenta o alvo `bench` como *"one cell, end to end, exiting with the checker's code so that the loop of etapa 5 can stop on it"*
- a **decisão 93** deixou escrita a regra que este PR é o primeiro a obedecer: *"todo laço que precisa do código de saída — este, e o da matriz da etapa 5 — chama o script direto"*

Esta spec não constrói mecanismo. **Ela não acrescenta uma série, uma engine, uma rota nem uma migration.** Ela acrescenta um laço, uma mudança de uma linha no gerador de carga, e o agregador que transforma 37 diretórios em uma tabela que a etapa 6 pode publicar — ou em uma recusa nomeada, quando a matriz não vale.

É também a primeira vez que o projeto produz um artefato cujo valor depende de **coisas que não aconteceram**: nenhum commit diferente entre as células, nenhuma árvore suja, nenhuma célula faltando, nenhum caos misturado. Metade do código deste PR existe para reprovar a própria matriz.

## Objetivo

Rodar as 36 células da matriz de forma automatizada e reprodutível, mais a célula-controle, e agregar o resultado num artefato único que a etapa 6 lê.

O sistema deve:

- Executar as 36 combinações de `estratégia × contenção × cenário × política de retentativa`, cada uma pelo `bench/run-cell.sh` que já existe, sem alterar o que aquela célula faz
- Trocar a engine sob teste entre células e **provar** que trocou, antes de medir
- Parar na primeira célula que não sair verde, distinguindo invariante violado de célula não verificável de threshold estourado
- Repetir a primeira célula por último e transformar a divergência entre as duas em um número com veredito
- Recusar-se a publicar uma matriz incoerente, nomeando a incoerência
- Produzir `matrix.json` e `matrix.md` a partir de `env.json`, `client.json` e `checker.json` — e de nada além disso
- Poder ser retomada de onde parou, dentro do mesmo commit

## Fora de Escopo

- **Sweep de pool do pessimista.** É a spec 02 desta etapa. Aqui o pool fica fixo em 25 nas 36 células, e o agregador reprova a matriz se ele variar (decisão 105)
- **Dashboards do Grafana.** É a spec 03. Este PR não toca `deploy/`
- **Escrever os números em `docs/`.** É a etapa 6. Este PR produz o artefato; publicar o resultado é outro trabalho, com outro tipo de revisão
- **Consertar o que a matriz encontrar.** Uma engine que viola invariante sob 1000 VUs é um achado, e achado vira spec própria. Este PR entrega o instrumento, não o conserto
- **Mexer no apostador.** `MAX_RETRIES`, `BID_DEADLINE`, o modelo de duplicatas e a política de backoff ficam como estão. A única mudança em `bench/bid-storm.js` é o campo `durationMs` no `client.json` (RF06)
- **Série nova, engine nova, migration, rota, painel, WebSocket.** Nada disso muda o gráfico deste PR
- **Prometheus como fonte.** O agregador nunca consulta o Prometheus, pelo mesmo motivo que o `cmd/checker` nunca consultou (decisão 4)

## Fluxo

```text
bench/run-matrix.sh
  │
  ├── pré-voo (uma vez, antes da primeira célula)
  │     ├─ árvore limpa: git status --porcelain vazio           → senão aborta
  │     ├─ jq, docker compose, curl presentes                    → senão aborta
  │     ├─ postgres, redis e closerd de pé                       → senão aborta
  │     └─ bench/results/$MATRIX livre, ou do mesmo commit e RESUME=1
  │
  ├── para cada uma das 36 células, na ordem do plano
  │     │   (contenção fora → cenário → política → ESTRATÉGIA dentro)
  │     │
  │     ├─ RESUME=1 e checker.json com exit 0?  → pula, imprime "pulada"
  │     ├─ senão: rm -rf do diretório da célula (nada de artefato meio escrito)
  │     │
  │     ├─ STRATEGY=<da célula> docker compose up -d --build auctiond
  │     ├─ wait_ready(auctiond) · wait_ready(closerd) · wait_strategy(<da célula>)
  │     │        └─ bench/wait.sh, o mesmo arquivo que chaos/run-all.sh lê
  │     │
  │     └─ RUN=$MATRIX/NN-<estratégia>-a<N>-<cenário>-<política> bench/run-cell.sh
  │              reset → seed → vacuum → flushall → warmup → reset → CARGA → checker
  │                    └─ código 0 segue · 1, 2 ou 99 PARA a matriz aqui
  │
  ├── célula 37: a célula 01 outra vez, por último
  │
  └── bin/matrix -dir bench/results/$MATRIX
        ├─ lê env.json + client.json + checker.json das 37
        ├─ nove recusas → exit 2, nomeando qual
        ├─ qualquer célula com failures > 0 → exit 1
        └─ publica: matrix.json + matrix.md + a tabela no stdout
```

Nada abaixo de `bench/run-cell.sh` muda de comportamento. A célula que a matriz roda é, byte a byte, a célula que a etapa 4 deixou pronta — exceto pelo nome do contêiner do k6, que passa a aceitar um `RUN` com subdiretório (RF07).

## Decisoes Tecnicas

### A estratégia é o eixo mais interno, e o `auctiond` é recriado a cada célula

A ordem de execução é **contenção → cenário → política → estratégia**, com a estratégia variando a cada linha. As três engines que serão comparadas num ponto do gráfico rodam adjacentes no tempo, dentro da mesma janela de minutos.

A alternativa óbvia — agrupar por estratégia, para recriar o `auctiond` três vezes em vez de 37 — economiza uns nove minutos e **confunde estratégia com tempo**. Se a máquina esquentar, se o cache de página encher, se qualquer coisa derivar ao longo de uma hora e meia, a deriva entra no gráfico com nome de estratégia. É exatamente o efeito de ordem que a célula-controle existe para detectar, agravado de propósito.

O recreate por célula compra uma segunda coisa, que o agrupamento perderia: o **processo** também parte do mesmo estado. Heap, pool aberto, janela de supressão em memória do fechamento (decisão 70) e os inboxes do shard nascem do zero em toda célula, e não herdados da célula anterior. A decisão 13 mandou resetar o banco entre células; isto é a mesma regra aplicada ao processo.

Custo aceito: ~15 s por recreate, ~9 minutos no total. É barato ao lado do que compra.

### A célula-controle é a primeira repetida por último, e a divergência tem banda

A célula 37 é a célula 01 outra vez: `optimistic`, 1 leilão, `ramp`, `immediate`. O agregador compara `aceitos/s` das duas e publica a divergência relativa.

| Divergência | Veredito | O que significa |
| --- | --- | --- |
| < 10% | OK | Ruído de máquina, indistinguível de efeito de ordem neste projeto |
| 10% a 25% | AVISO | A matriz é publicável, e a etapa 6 publica esta divergência ao lado do gráfico |
| > 25% | RECUSA | A matriz não vale: uma diferença desse tamanho entre estratégias não pode ser atribuída ao mecanismo |

A banda existe porque o controle **não separa deriva de ruído** — ele mede os dois somados. Fingir que uma diferença de 3% é efeito de ordem seria tão desonesto quanto ignorar uma de 30%. O corte em 25% tem um critério: acima dele, a distância entre duas curvas do gráfico principal pode ser inteiramente explicada pela ordem de execução, e o entregável do projeto deixa de significar alguma coisa.

### O agregador lê três arquivos, e nunca o Prometheus nem o `summary.json`

`env.json`, `client.json` e `checker.json`. Ponto.

A regra vem da decisão 4 e do comentário que abre `cmd/checker/client.go`: o `summary.json` do k6 tem formato interno ao k6, que muda entre versões e **muda em silêncio** — um campo que vira `null` em vez de sumir produziria uma taxa verde contra um zero. O `client.json` existe precisamente para ser o meio-termo estável, e o agregador herda o contrato inteiro, inclusive os ponteiros: ausente nunca é lido como zero.

O Prometheus continua de pé porque faz parte do ambiente fixado, e a spec 03 vai desenhar em cima dele. Ele só não é fonte de número publicado.

### `durationMs` entra no `client.json`, e é a única mudança no gerador

Taxa por segundo precisa de janela, e nenhuma das janelas que já existem serve: `startedAt`/`finishedAt` do `env.json` cobrem reset, seed, `VACUUM`, warmup, segundo reset e o checker — para uma célula de `last_second_spike` isso é 66 segundos de relógio em cima de 15 segundos de carga, e `aceitos/s` sairia com um quarto do valor real.

A janela honesta é a do k6, e o k6 a entrega em `data.state.testRunDurationMs`. Ela vira um campo do `client.json`, lida com a mesma defesa que `count` e `trend` já usam: se não for número, **estoura**, em vez de virar zero três passos adiante.

O `cmd/checker` não passa a exigir o campo, e isso é deliberado: o checker exige o que lê, e ele não lê duração. Quem exige é o agregador, que recusa a matriz inteira se faltar. É a separação de camadas do projeto dita de novo — o checker julga corretude, a matriz julga publicabilidade.

### Tentativas por aceito é `attempts ÷ accepted`, e não o trend que já existe

O `client.json` publica `clientAttemptsPerAccept`, um trend amostrado **no aceite**. A coluna da matriz não usa esse número, e a razão é o defeito que ele tem embutido:

> Sob alta contenção, quem tenta muito e desiste não é amostrado — a desistência cai em `bids_exhausted` e o trend nunca a vê. O resultado é que **o trend cai quando a disputa sobe**, que é o oposto do que a coluna promete ao leitor.

A razão global entre os dois contadores não tem esse viés: `attempts` conta toda tentativa feita, `accepted` conta todo aceite, e a divisão é a amplificação real do cliente. O trend fica no `client.json`, onde já está, e serve para outra pergunta — a distribuição da amplificação **entre os que conseguiram**. Publicar os dois com o mesmo nome seria a forma mais eficiente de alguém plotar o errado.

`bids_exhausted` continua sendo coluna própria, e é ela que carrega o que o trend esconde.

### Sob caos o `99` é evidência; na matriz ele para tudo

`http_req_failed: rate<0.01` estourado faz o k6 sair 99, e o `run-cell.sh` só tolera isso quando `CHAOS` está setado (decisão 83). Na matriz, `CHAOS` é vazio nas 36 células e o 99 continua abortando.

É a leitura que a etapa 1 escreveu e que continua valendo: acima de 1% de erro **real** — 5xx, 404, 400, transporte — o que quebrou foi a medição, não a engine. 409 e 422 não contam, 410 e 425 não contam, e um apostador que desiste no `BID_DEADLINE` não gera erro nenhum. Sobra infraestrutura caindo, e uma linha de resultado publicada em cima disso seria uma mentira com unidade.

O I6 concorda pelo mesmo número (`maxErrorRate = 0.01`), então a célula seria reprovada duas vezes. A matriz para na primeira.

**O que fazer quando acontecer:** é achado, não é ajuste de threshold. Se a engine pessimista sob 1000 VUs devolve 503 acima de 1%, isso é o limite dela aparecendo — e merece uma spec que o meça de propósito, com o erro como variável e não como acidente. Afrouxar o threshold para a matriz terminar seria escolher terminar em vez de medir.

### São 36 e não 24: a política de retentativa vale para as três engines

A tentação é dizer que `pessimistic` e `shard` não produzem 409, logo `immediate` e `jitter` dariam a mesma célula duas vezes, logo a matriz teria 24 células.

Está errado, e o motivo está no apostador: o backoff é aplicado **também no 422**. Ser superado é o desfecho comum das três engines, e nas três o cliente re-mira e espera antes de tentar de novo. Trocar `immediate` por `jitter` muda a carga oferecida nas três, não só na otimista.

O que continua verdade é o que `benchmark.md` já diz: a política **importa muito mais** na otimista, porque lá ela também governa o 409. A matriz mede o tamanho dessa diferença em vez de assumi-lo.

### O que é constante entre contenções é a carga oferecida, não a entregue

`benchmark.md` diz "mesmo volume total de requisições" nos três níveis de contenção. Dito com precisão: o que é idêntico é o **cenário** — mesmos VUs, mesma duração, mesmo apostador. O número de requisições entregues não é idêntico, e não pode ser: sob contenção alta a latência sobe e cada VU completa menos iterações.

Isso não é defeito do desenho, é o desenho. Se o harness forçasse o mesmo número de requisições entregues, ele estaria fechando a variável dependente na mão e o gráfico mediria a paciência do gerador. `aceitos/s` é comparável porque a carga oferecida é idêntica — e a diferença entre oferecido e entregue é exatamente o que `bids_exhausted` e a razão de tentativas contam.

A spec registra isso porque a alternativa é alguém ler a tabela em 2027 e concluir que houve erro de método.

### A matriz recusa em vez de publicar linha duvidosa

Nove condições impedem a publicação, e cada uma existe porque a linha correspondente seria plausível e falsa:

| # | Recusa | Por que a linha seria falsa |
| --- | --- | --- |
| R1 | Célula faltando, ou diretório fora do plano | Uma matriz de 35 células com uma lacuna não desenha curva; e um diretório extra é uma célula de origem desconhecida |
| R2 | `checker.json` com `exit: 2` | Célula não verificada não é resultado (decisão 93) |
| R3 | Bloco `chaos` presente | Célula de caos mediu outro sistema (decisão 92) |
| R4 | `git.commit` divergente, ou `dirty: true` | Duas células de códigos diferentes não são a mesma matriz |
| R5 | `env.json.cell` diferente do nome do diretório | A célula rodou contra outra engine, ou o diretório foi reaproveitado |
| R6 | `poolSize` divergente | O pool é o sweep da spec 02, e aqui é constante (decisão 105) |
| R7 | `durationMs` ausente ou ≤ 0 | Toda taxa por segundo sairia zero ou infinita |
| R8 | `accepted == 0` | Toda taxa da linha sai zero e `attempts ÷ accepted` é divisão por zero |
| R9 | Divergência do controle > 25% | Efeito de ordem grande o bastante para inverter o gráfico |

`checker.json` com `failures > 0` sai com **1**, não com 2: é um resultado sobre a engine, e a distinção é a mesma que o checker mantém desde a etapa 1.

Cinco delas — R1, R3, R4, R5 e R6 — são recusas que **o checker é incapaz de fazer**, e não por descuido: ele verifica uma célula por vez, contra um banco que acabou de ser resetado, e não tem como saber o nome do diretório em que vai cair, qual commit produziu a célula vizinha ou o que as outras 36 fizeram. R5 é o caso extremo: uma célula perfeitamente verde, cujo `env.json` diz `pessimistic` e cujo diretório diz `optimistic`, vira uma linha plausível e falsa na tabela — e a única camada capaz de notar é esta.

R8 é a mais barata das nove e a menos provável de disparar: com zero aceites o trend `seq_seen` não tem amostra, o `handleSummary` estoura e o `client.json` nem chega a existir, o que já derrubaria a célula em exit 2. Ela existe porque a linha custa uma comparação e o que ela evita é uma divisão por zero na coluna de tentativas por aceito.

### Retomada só dentro do mesmo commit, e a célula refeita perde os artefatos antigos

Uma matriz leva entre uma hora e meia e duas horas. Perder tudo por causa da célula 30 é caro o bastante para justificar `RESUME=1`.

O que a retomada pode pular: uma célula cujo `checker.json` existe e tem `exit: 0`. Nada mais. Célula sem `checker.json`, com código diferente de zero ou com JSON ilegível é **refeita**, e o diretório é apagado antes — um `client.json` sobrevivente de um k6 que morreu no meio seria verificado contra um banco que outra célula escreveu.

O que a retomada nunca pode fazer é atravessar commit. O pré-voo lê o `env.json` de qualquer célula já pronta e compara com o `HEAD`; divergiu, aborta no segundo zero em vez de descobrir isso 90 minutos depois, no agregador. É a mesma recusa R4, movida para onde ela custa menos.

### As duas esperas do compose viram um arquivo, lido pelos dois laços

`wait_ready` e `wait_strategy` existem hoje dentro de `chaos/run-all.sh`. A matriz precisa das duas, com a mesma semântica.

Copiar 25 linhas seria o mais barato de escrever e o mais caro de manter no único lugar onde importa: `wait_strategy` codifica o fato de que `bid_confirm_duration_seconds_count{strategy="..."}` é a **prova** de qual engine o processo está rodando — não o que o compose pediu, o que o processo faz. No dia em que esse nome de série mudar, a cópia esquecida trava 90 segundos e derruba uma matriz de duas horas pela metade.

Então as duas funções mudam para `bench/wait.sh`, e `chaos/run-all.sh` passa a lê-lo. O preço é que este PR precisa provar que não quebrou o caos, e paga esse preço com um cenário de caos rodado no checkpoint C6.

### `make matrix` é para gente; automação chama o script

Aplicação direta da decisão 93, e é a segunda vez que ela é aplicada: o alvo do `Makefile` é uma linha que chama `bench/run-matrix.sh`, porque o GNU make colapsa qualquer falha de recipe no seu próprio código 2 e apagaria a diferença entre "invariante violado" (1), "não verificável" (2) e "threshold estourado" (99) — que é justamente a informação de que quem retoma a matriz precisa.

### Idempotencia

A matriz não introduz idempotência nova; ela herda a que já existe e depende dela em três pontos.

**Dentro da célula:** o `run-cell.sh` faz `TRUNCATE`, seed, `VACUUM ANALYZE` e `FLUSHALL` antes do warmup e outra vez antes da carga medida. Toda célula, inclusive uma refeita pela retomada, abre exatamente sobre o mesmo estado — banco vazio, estatísticas frescas, Redis sem marca de idempotência sobrevivente. É o que torna a ordem irrelevante dentro do laço e a retomada segura.

**No laço:** rodar `run-matrix.sh` duas vezes com `RESUME=1` no mesmo `MATRIX` converge — a segunda execução pula tudo o que já está verde e para no mesmo lugar, ou termina. Rodar sem `RESUME=1` sobre um `MATRIX` existente é recusado, não silenciosamente sobrescrito.

**No agregador:** `bin/matrix` é uma função pura dos 37 diretórios. Ele nunca escreve dentro de um diretório de célula, nunca consulta banco, nunca fala com a rede, e rodar duas vezes produz o mesmo `matrix.json`, byte a byte, salvo o campo `generatedAt`.

### Observabilidade

**Zero série nova.** Como na spec 02 da etapa 4, e pelo mesmo motivo: tudo de que a matriz precisa já foi instrumentado pelas etapas 1 a 4, e este PR só lê.

A matriz também não lê métrica: ela lê três arquivos JSON. A única chamada a `/metrics` do PR inteiro é o `wait_strategy`, e ela não colhe número — ela confirma qual engine o processo está rodando antes de a medição começar.

O `matrix.md` é o formato de entrega para a etapa 6, que é um PR de `docs/`. As colunas são exatamente as da tabela **Resultados** de [benchmark.md](../../projeto/benchmark.md), mais as duas que aquela tabela não previa e a matriz precisa carregar: o número de avisos do checker por célula e a divergência do controle.

## Requisitos Funcionais

### RF01 - `bench/run-matrix.sh`: o plano das 36 células

O plano é fixo, declarado no script, e a ordem é a da decisão 94: contenção fora, cenário, política, e **estratégia dentro**.

| Eixo | Valores | Posição |
| --- | --- | --- |
| Contenção (`AUCTIONS`) | 1, 10, 1000 | mais externo |
| Cenário (`SCENARIO`) | `ramp`, `last_second_spike` | |
| Política (`POLICY`) | `immediate`, `jitter` | |
| Estratégia (`STRATEGY`) | `optimistic`, `pessimistic`, `shard` | mais interno |

Nome de cada célula: `NN-<estratégia>-a<leilões>-<cenário>-<política>`, com `NN` de `01` a `36` na ordem de execução. As doze primeiras:

```text
01-optimistic-a1-ramp-immediate
02-pessimistic-a1-ramp-immediate
03-shard-a1-ramp-immediate
04-optimistic-a1-ramp-jitter
05-pessimistic-a1-ramp-jitter
06-shard-a1-ramp-jitter
07-optimistic-a1-last_second_spike-immediate
08-pessimistic-a1-last_second_spike-immediate
09-shard-a1-last_second_spike-immediate
10-optimistic-a1-last_second_spike-jitter
11-pessimistic-a1-last_second_spike-jitter
12-shard-a1-last_second_spike-jitter
```

De `13` a `24` a mesma sequência com `a10`, de `25` a `36` com `a1000`. A célula 37 é `37-control-optimistic-a1-ramp-immediate`.

Constantes nas 36: `ENDS_IN=30m` (nada fecha no meio da célula), `MIN_INCREMENT=100`, `DB_POOL_SIZE` do `.env`, `CHAOS` vazio.

O identificador da matriz é `MATRIX`, com default `m$(date -u +%Y%m%dT%H%M%S)`, e o `RUN` de cada célula é `$MATRIX/<nome>`. Ele nunca começa com `chaos-` (decisão 92).

Duas opções de linha de comando, e nenhuma outra:

- `--dry-run`: imprime as 37 linhas do plano na ordem, com o `RUN` completo de cada uma, e sai 0 sem tocar em nada
- `--only <regex>`: roda apenas as células cujo nome casa com a regex. Não suprime a agregação — a recusa que vem depois é o comportamento correto e é o que o C3 demonstra

### RF02 - `bench/run-matrix.sh`: o pré-voo

Antes da primeira célula, e tudo aborta com mensagem própria e código 2:

1. `git status --porcelain` vazio. Uma matriz medida de árvore suja não é reproduzível, o `env.sh` gravaria `dirty: true` em toda célula e o agregador recusaria duas horas depois (R4)
2. `jq`, `curl` e `docker compose` disponíveis
3. `docker compose ps` mostra `postgres` e `redis` de pé; `docker compose start closerd` é executado e o `closerd` precisa responder `/readyz` 200. Ele participa de toda célula desde a decisão 80
4. Se `bench/results/$MATRIX` já existe: só é aceito com `RESUME=1`, e só se o `env.json` de alguma célula já pronta trouxer o mesmo `git.commit` do `HEAD`. Divergiu, aborta
5. O plano é impresso antes de a primeira célula rodar, com a estimativa de tempo

### RF03 - `bench/run-matrix.sh`: a troca de engine, provada antes de medir

Para cada célula, nesta ordem:

```bash
STRATEGY="$strategy" docker compose up -d --build auctiond
wait_ready  "$AUCTIOND"     # /readyz 200, deadline de 90s
wait_ready  "$CLOSERD"      # /readyz 200, deadline de 90s
wait_strategy "$strategy"   # bid_confirm_duration_seconds_count{strategy="..."}
```

`up -d --build` e não `start`: a estratégia é variável de ambiente do contêiner, e trocá-la exige recriar. `wait_strategy` é obrigatório e não é redundante com `wait_ready`: o `/readyz` pode responder 200 um instante antes de o decorador ligar a série com o rótulo da estratégia, e o pré-voo do próprio `run-cell.sh` abortaria a célula. Esperar aqui é a diferença entre o laço aguardar e o laço falhar.

### RF04 - `bench/run-matrix.sh`: a retomada

Com `RESUME=1`, para cada célula do plano:

- Existe `bench/results/$MATRIX/<nome>/checker.json` legível e com `.exit == 0` → pula, imprime `pulada`, não recria o `auctiond`
- Qualquer outro estado → o diretório é apagado e a célula roda inteira

O apagamento é guardado: o caminho precisa estar sob `bench/results/$MATRIX/`, o nome precisa ser um dos 37 do plano, e `$MATRIX` precisa ser não vazio. Um `rm -rf` de caminho computado sem guarda é a única linha deste PR capaz de destruir trabalho alheio.

Sem `RESUME=1`, um `$MATRIX` existente é recusado (RF02.4).

### RF05 - `bench/run-matrix.sh`: os códigos de saída

O laço para na **primeira** célula com código diferente de zero, imprime o nome da célula, o código, e o comando exato de retomada. O código sai do script sem tradução.

| Código | Origem | O que significa | O que fazer |
| --- | --- | --- | --- |
| 0 | tudo verde + agregador publicou | A matriz existe | Etapa 6 |
| 1 | `cmd/checker` | Invariante violado numa célula | Achado sobre a engine: ler, e virar spec |
| 2 | `cmd/checker`, pré-voo ou agregador | Célula não verificável, ou matriz não publicável | Consertar a causa e retomar |
| 99 | k6 | `http_req_failed` acima de 1% | Achado sobre o limite do sistema (decisão 102) |
| 130 | interrupção | Ctrl-C | Retomar com `RESUME=1` |

### RF06 - `bench/bid-storm.js`: `durationMs` no `client.json`

Único campo novo, e única mudança no gerador:

```js
durationMs: duration(data),
```

```js
// The window the rates are computed over, and the only honest one: env.json's
// startedAt..finishedAt covers reset, seed, vacuum, warmup and the checker,
// which for a 15s spike is four times the load itself.
function duration(data) {
  const ms = data.state && data.state.testRunDurationMs;
  if (typeof ms !== 'number' || !(ms > 0)) throw new Error('k6 summary has no testRunDurationMs');
  return ms;
}
```

Estoura em vez de devolver zero, pela mesma regra que `count` e `trend` já seguem. Nada mais no arquivo muda: nem cenário, nem threshold, nem apostador, nem duplicatas, nem `summaryTrendStats`.

O `cmd/checker` **não** passa a exigir o campo (decisão 97).

### RF07 - `bench/run-cell.sh`: o nome do contêiner saneado

Uma linha:

```bash
K6_NAME="bid-storm-k6-$(printf '%s' "$RUN" | tr -c '[:alnum:]_.-' '-')"
```

O `RUN` da matriz carrega subdiretório (`m2026.../01-optimistic-a1-ramp-immediate`) e nome de contêiner do Docker não aceita barra. `RESULTS`, `mkdir -p`, o `RESULTS_DIR` do k6 e o `-run` do checker já funcionam com subdiretório.

Para todo `RUN` sem caractere especial — os de hoje, os do caos, os que uma pessoa digita — o saneamento é a identidade e o nome do contêiner é literalmente o mesmo de antes. Nada mais no arquivo muda.

### RF08 - `bench/wait.sh`, e o `chaos/run-all.sh` lendo o mesmo arquivo

`bench/wait.sh` passa a conter `code_of`, `wait_ready` e `wait_strategy`, movidas de `chaos/run-all.sh` sem mudança de comportamento: mesmos deadlines de 90 s, mesmo intervalo de 2 s, mesmas mensagens.

Ele é `source`ado por `bench/run-matrix.sh` e por `chaos/run-all.sh`. Não é executável, não tem `set -euo pipefail` próprio, e depende de `AUCTIOND_URL`/`CLOSERD_URL` já resolvidos por quem o lê — como já acontece hoje dentro do `run-all.sh`.

`check_cure` **não** se move: ela é do caos, e a matriz não tem o que curar.

### RF09 - `cmd/matrix`: a leitura e as nove recusas

```text
bin/matrix -dir bench/results/<MATRIX> [-out <dir>]
```

Lê, de cada subdiretório: `env.json`, `client.json` e `checker.json`. Nada mais — nem `summary.json`, nem `checker.txt`, nem Prometheus.

Todo contador do `client.json` é ponteiro, pelo motivo já escrito em `cmd/checker/client.go`: ausente não pode ser lido como zero.

As nove recusas da tabela da seção de decisões técnicas, implementadas nesta ordem e **todas reportadas**, não só a primeira: uma matriz com três problemas deve dizer os três, ou o operador descobre um por execução.

| Código | Quando |
| --- | --- |
| 0 | Publicável, com ou sem avisos |
| 1 | Alguma célula com `failures > 0` no `checker.json` |
| 2 | Qualquer uma das nove recusas |

### RF10 - `cmd/matrix`: as colunas e as fórmulas

| Coluna | Fórmula | Por que assim |
| --- | --- | --- |
| VUs no pico | `ramp` → 500, `last_second_spike` → 1000 | Deriva do cenário; cenário desconhecido é recusa |
| Aceitos/s | `accepted ÷ (durationMs/1000)` | A janela é a do k6 (decisão 97) |
| Conflitos/s | `conflict ÷ (durationMs/1000)` | Só a otimista produz 409; nas outras a coluna é 0 e isso é informação |
| p95 confirmação | `confirmLatencyMs.p95` | Lado cliente, medido pelo k6. O lado servidor é a spec 03 |
| Tentativas/aceito | `attempts ÷ accepted` | Global, sem o viés do trend (decisão 98) |
| Exauridos | `exhausted ÷ (accepted + exhausted)` | Fração dos lances lógicos que desistiram |
| Avisos | `checker.json.warnings` + os da matriz | Aviso nunca é escondido (decisão 106) |

Avisos que a matriz acrescenta por célula, sem reprovar:

- `gerador saturado`: `env.json.generator.saturated` verdadeiro. O I6 já avisa; a matriz carrega para a tabela, porque um `aceitos/s` medido com o k6 no teto pode ser do k6
- `exaustão acima de 20%`: o limiar de `benchmark.md`. Acima dele, `MAX_RETRIES` está mascarando o efeito — e **subir `MAX_RETRIES` obriga a rodar as 36 de novo**, porque uma matriz com dois apostadores diferentes não é uma matriz
- `controle divergente`: entre 10% e 25%

### RF11 - `cmd/matrix`: a célula-controle

Compara `aceitos/s` de `01-...` e `37-control-...`, publica valor absoluto e relativo, e aplica as bandas da decisão 95: < 10% OK, 10–25% AVISO, > 25% recusa R9.

O bloco vai para o `matrix.json` e para o rodapé do `matrix.md`, sempre — inclusive quando OK. Um controle que só aparece quando falha é um controle que o leitor não sabe se rodou.

### RF12 - `matrix.json` e `matrix.md`

`matrix.json` é a autoridade:

```json
{
  "matrix": "m20260824T193000",
  "generatedAt": "2026-08-24T21:31:07Z",
  "git": { "commit": "…", "dirty": false },
  "poolSize": 25,
  "host": { "kernel": "…", "cpus": 16, "memoryBytes": 0 },
  "limits": { "auctiond": {}, "postgres": {}, "redis": {}, "closerd": {}, "k6": {} },
  "cells": [
    {
      "order": 1,
      "run": "m20260824T193000/01-optimistic-a1-ramp-immediate",
      "strategy": "optimistic", "auctions": 1,
      "scenario": "ramp", "policy": "immediate", "peakVUs": 500,
      "durationMs": 120431,
      "accepted": 0, "acceptedPerSecond": 0.0,
      "conflictPerSecond": 0.0, "confirmP95Ms": 0.0,
      "attemptsPerAccept": 0.0, "exhaustedRate": 0.0,
      "checker": { "warnings": 0, "failures": 0 },
      "warnings": []
    }
  ],
  "control": {
    "cell": 1, "repeat": 37,
    "acceptedPerSecond": [0.0, 0.0],
    "divergence": 0.0, "verdict": "OK"
  },
  "warnings": [],
  "publishable": true
}
```

`host` e `limits` saem do `env.json` de uma célula qualquer — são idênticos nas 37 por construção, e R4 garante isso.

`matrix.md` é a mesma coisa como tabela markdown, com as 36 linhas na ordem do plano, o bloco do controle no rodapé e a lista de avisos. É o formato que a etapa 6 cola em `benchmark.md`.

### RF13 - `Makefile`

```make
# The 36 cells and the control, ~1h30. One line, and for the same reason as
# `make chaos`: the loop lives in the script because make collapses every exit
# code into its own 2, which is exactly the difference the matrix needs to keep
# between a violated invariant (1), a cell it could not verify (2) and a
# breached threshold (99) — decisão 93.
matrix:
	bench/run-matrix.sh
```

`.PHONY` ganha `matrix`. `make matrix RESUME=1` e `make matrix MATRIX=... ` funcionam pelo `export` que já está no topo do arquivo.

### RF14 - Testes

`cmd/matrix` ganha suíte de unidade. Os casos, todos construindo a matriz sintética em `t.TempDir()`:

| Caso | Espera |
| --- | --- |
| 37 células coerentes | exit 0, `publishable: true`, 36 linhas, controle OK |
| Célula 19 faltando | exit 2, mensagem nomeando `19-…` |
| Diretório fora do plano | exit 2, nomeando o intruso |
| `checker.json` com `exit: 2` | exit 2 |
| `checker.json` com `failures: 1` | exit **1** |
| Bloco `chaos` presente | exit 2 |
| Commit divergente em uma célula | exit 2 |
| `dirty: true` em uma célula | exit 2 |
| `env.json.cell.strategy` ≠ nome do diretório | exit 2 |
| `poolSize` 25 em 36 e 50 em uma | exit 2 |
| `durationMs` ausente / zero | exit 2 |
| `accepted: 0` | exit 2 |
| Divergência do controle 12% | exit 0, com aviso |
| Divergência do controle 30% | exit 2 |
| Exaustão 25% numa célula | exit 0, com aviso na linha |
| `generator.saturated: true` | exit 0, com aviso na linha |
| Três problemas ao mesmo tempo | exit 2 relatando os três |
| Duas execuções seguidas | `matrix.json` idêntico exceto `generatedAt` |

Nenhum `testdata/` com 37 diretórios entra no repositório: um helper monta a matriz em memória e escreve os JSON no tempdir, e é ele que os casos mutam.

Os scripts não ganham teste automatizado, pela razão que a spec 02 da etapa 4 já registrou: um harness de shell que sobe contêineres para testar o script que sobe contêineres é mais código do que o script. O aceite deles é C2, C3 e C6.

### RF15 - O resto do sistema não muda

`git diff --name-only` precisa sair vazio para: `internal/`, `cmd/auctiond/`, `cmd/closerd/`, `cmd/seed/`, `cmd/checker/`, `migrations/`, `deploy/`, `docker-compose.yaml`, `Dockerfile`, `bench/env.sh`, `.env.example`, `go.mod`, `go.sum`.

Em particular: **nenhuma série nova**, nenhuma engine tocada, nenhum dashboard, nenhum `if` de matriz dentro de binário do sistema. `grep -rn 'MATRIX' internal/ cmd/auctiond cmd/closerd cmd/seed cmd/checker` sai vazio.

## Requisitos Nao Funcionais

- A execução completa precisa rodar **sem supervisão**, entre uma hora e meia e duas horas, numa máquina ociosa. Nada além do compose do projeto rodando no host
- O laço nunca escreve no banco por conta própria: quem reseta e semeia é o `run-cell.sh`, como já faz
- Nenhuma decisão de uma célula depende do resultado da anterior, exceto a decisão de parar
- `shellcheck` sem aviso em `bench/run-matrix.sh`, `bench/wait.sh`, `bench/run-cell.sh` e `chaos/run-all.sh`
- `gofmt -l .` vazio, `go vet ./...` limpo, `go test ./... -race` verde
- O agregador não fala com a rede nem com o banco, e roda em menos de um segundo sobre 37 diretórios
- O `matrix.json` precisa ser legível por `jq` sem nenhum passo intermediário: é o insumo da etapa 6

## Budget do PR

Até 9 arquivos e aproximadamente 800 linhas, shell incluído.

O peso está dividido: o laço é o maior arquivo de shell do projeto depois do injetor, e o agregador carrega nove recusas com mensagem própria mais a suíte que as exercita. O Go continua sendo a parte testável e o shell continua sendo a parte orquestradora, como no PR do caos.

Se o PR passar de 9 arquivos ou 800 linhas, **pare e reporte**. O corte é o `matrix.md`, nesta ordem: o `matrix.json` é a autoridade e a tabela é derivável dele com uma linha de `jq`, que fica registrada aqui como plano B:

```bash
jq -r '.cells[] | [.order, .strategy, .auctions, .scenario, .policy,
       .peakVUs, .acceptedPerSecond, .confirmP95Ms, .attemptsPerAccept,
       .exhaustedRate] | @tsv' matrix.json
```

O corte seguinte, se ainda faltar, é `RESUME`: sem ele uma matriz interrompida custa duas horas para refazer, o que é caro mas não é errado.

Se a conta estourar por causa de `internal/`, pare bem mais cedo: significa que a matriz entrou no processo, e não existe motivo nenhum para isso acontecer.

## Claude Code

- Modelo: `claude-opus-5`
- Esforco: alto
- Referencia permitida: `docs/projeto/benchmark.md`, `docs/projeto/provas.md`, `docs/projeto/arquitetura.md`, `docs/decisoes/etapa-1.md`, `docs/decisoes/etapa-2.md`, `docs/decisoes/etapa-3.md`, `docs/decisoes/etapa-4.md`, `docs/decisoes/etapa-5.md`, `docs/specs/etapa-4/02-spec-caos.md`, `docs/specs/etapa-5/01-spec-matriz.md`

Prompt:

```text
Implemente docs/specs/etapa-5/01-spec-matriz.md no repositorio bid-storm.

Leia antes de comecar:
  docs/specs/etapa-5/01-spec-matriz.md  (a spec — a autoridade)
  docs/decisoes/etapa-5.md              (o porque; decisoes 94 a 109)
  docs/projeto/benchmark.md             (a matriz e a tabela de resultados)
  bench/run-cell.sh                     (o tijolo; muda UMA linha)
  chaos/run-all.sh                      (o laco irmao: wait_ready e wait_strategy saem daqui)
  bench/bid-storm.js                    (handleSummary: entra UM campo)
  cmd/checker/client.go                 (o contrato do client.json, e os ponteiros)
  cmd/checker/main.go                   (os codigos 0, 1 e 2)

Escopo: apenas RF01..RF15. NAO implemente sweep de pool, dashboard, painel,
serie nova, engine nova, migration, nem correcao de qualquer defeito que a
matriz encontrar.

Regras:
- Modulo: github.com/samuka7abr/bid-storm
- NAO altere internal/, cmd/auctiond/, cmd/closerd/, cmd/seed/, cmd/checker/,
  migrations/, deploy/, docker-compose.yaml, Dockerfile, bench/env.sh,
  .env.example, go.mod nem go.sum. Dentro de cmd/, so cmd/matrix/.
- ZERO series novas. Se a matriz parecer precisar de instrumentacao, pare e
  reporte: e achado sobre a etapa 3 ou 4, nao trabalho desta spec.
- Em bench/bid-storm.js muda UM campo: durationMs. Cenario, threshold,
  apostador, duplicatas e summaryTrendStats ficam byte a byte iguais.
- Em bench/run-cell.sh muda UMA linha: o saneamento de K6_NAME. Para um RUN
  sem barra o nome do conteiner tem de sair identico ao de hoje.
- O agregador NUNCA fala com Prometheus, com o banco nem com a rede, e nunca
  le summary.json. Tres arquivos: env.json, client.json, checker.json.
- Todo contador lido do client.json e ponteiro: ausente nao pode virar zero.
- O agregador relata TODAS as recusas que encontrar, nao so a primeira.
- failures > 0 sai 1. As nove recusas saem 2. Nunca o contrario.
- O rm -rf da retomada e guardado: caminho sob bench/results/$MATRIX, nome no
  plano, MATRIX nao vazio. Sem as tres guardas, nao escreva a linha.
- A ordem das celulas e contencao > cenario > politica > ESTRATEGIA. A
  estrategia e o eixo mais interno, e o auctiond e recriado a cada celula.
- Rode os checkpoints C1..C6 e cole a saida real de cada um. Checkpoint sem
  saida nao conta como aceito. C5 leva ~1h30: rode.
- Se estourar o budget de 9 arquivos / ~800 linhas, pare e reporte.
- Nao altere nada dentro de docs/.
```

## Arquivos Esperados

Criar:

```text
bench/run-matrix.sh          o plano, o pre-voo, o laco, a retomada, os codigos
bench/wait.sh                code_of, wait_ready, wait_strategy
cmd/matrix/main.go           leitura dos tres artefatos, as nove recusas, os codigos
cmd/matrix/report.go         as colunas derivadas, o controle, matrix.json e matrix.md
cmd/matrix/matrix_test.go    os dezoito casos da RF14, sobre t.TempDir()
```

Editar:

```text
bench/run-cell.sh            K6_NAME saneado (uma linha)
bench/bid-storm.js           durationMs no client.json (um campo)
chaos/run-all.sh             passa a ler bench/wait.sh no lugar das copias
Makefile                     make matrix, e .PHONY
```

## Testes

Adicionar:

```text
cmd/matrix/matrix_test.go    RF14, as recusas, os avisos, o controle e a idempotencia
```

Editar:

```text
(nenhum)
```

`cmd/checker` não é tocado por este PR, e nenhum teste existente muda: a matriz é uma camada acima do checker, e o dia em que ela exigir mudança no checker é o dia de parar e reportar.

## Checkpoints Mensuraveis

### C1 - Unidade, e a fronteira do diff

```bash
go test ./cmd/matrix/... -race -count=1 -v
go test ./... -race -count=1
gofmt -l . && go vet ./...
shellcheck bench/run-matrix.sh bench/wait.sh bench/run-cell.sh chaos/run-all.sh

git diff --name-only -- internal cmd/auctiond cmd/closerd cmd/seed cmd/checker \
  migrations deploy docker-compose.yaml Dockerfile bench/env.sh .env.example \
  go.mod go.sum
git diff --stat -- bench/run-cell.sh bench/bid-storm.js
grep -rn 'MATRIX' internal/ cmd/auctiond cmd/closerd cmd/seed cmd/checker \
  || echo "nenhum binario do sistema conhece a matriz"
```

Aceite:

- Os dezoito casos da RF14 passam, inclusive os dois que separam exit 1 de exit 2 e o que exige três recusas relatadas juntas
- A suíte inteira continua verde, com a conformidade das três engines intocada
- `git diff --name-only` não lista arquivo algum
- `bench/run-cell.sh` mostra **uma** linha alterada; `bench/bid-storm.js` mostra o campo e a função de leitura, e nada mais
- `grep -rn MATRIX` não encontra nada nos binários do sistema
- `shellcheck` sem aviso nos quatro scripts

### C2 - O plano, antes de qualquer carga

```bash
bench/run-matrix.sh --dry-run | tee /tmp/plano.txt
grep -c . /tmp/plano.txt
head -12 /tmp/plano.txt
tail -1  /tmp/plano.txt
# a estrategia alterna a cada linha, e a contencao muda de doze em doze
awk '{print $2}' /tmp/plano.txt | sed -n '1,6p'
grep -c 'a1000' /tmp/plano.txt
grep -c '^chaos' /tmp/plano.txt || echo "nenhuma celula com prefixo de caos"
```

Aceite:

- 37 linhas, na ordem da RF01
- As doze primeiras são exatamente as doze listadas na RF01, com a estratégia mudando a cada linha
- `a1000` aparece em 12 linhas
- A última é `37-control-optimistic-a1-ramp-immediate`
- Nenhuma linha começa com `chaos-`
- `--dry-run` não subiu contêiner, não escreveu em `bench/results/` e saiu 0

### C3 - Três células de verdade, a retomada, e a recusa

```bash
make up && sleep 20
MATRIX=c3 bench/run-matrix.sh --only '^0[1-3]-'; echo "exit=$?"

ls bench/results/c3
jq -r '.exit, .failures' bench/results/c3/01-optimistic-a1-ramp-immediate/checker.json
jq -r '.cell.strategy' bench/results/c3/0*/env.json
jq -r '.durationMs' bench/results/c3/0*/client.json
docker ps -a --format '{{.Names}}' | grep bid-storm-k6 || echo "nenhum k6 orfao"

# a retomada pula o que ja esta verde
MATRIX=c3 RESUME=1 bench/run-matrix.sh --only '^0[1-3]-' 2>&1 | grep -c pulada

# e o agregador recusa a matriz incompleta, nomeando o que falta
bin/matrix -dir bench/results/c3; echo "exit=$?"
```

Aceite:

- As três células saem verdes, uma por estratégia, em torno de dez minutos
- Cada `env.json` traz a estratégia do nome do diretório — `optimistic`, `pessimistic`, `shard`, nesta ordem: a troca de engine funcionou e foi provada antes de medir
- `durationMs` está presente e é próximo de 120000 nas três
- Nenhum contêiner do k6 sobrou
- A segunda execução imprime `pulada` três vezes e termina em segundos, sem recriar o `auctiond`
- O agregador sai **2** e nomeia as 34 células ausentes, inclusive a de controle. Esta recusa é o comportamento correto, e é o que o C4 vai plantar em cima

### C4 - As nove recusas, plantadas

```bash
cp -r bench/results/c3 bench/results/c4
# … monta uma matriz sintética de 37 células a partir das três reais,
#    e depois estraga uma cópia de cada vez com jq:
for defeito in commit-divergente dirty chaos-presente strategy-trocada \
               pool-divergente duration-zero accepted-zero controle-30 \
               celula-faltando; do
  bin/matrix -dir bench/results/c4-$defeito > /tmp/$defeito.txt 2>&1
  printf '%-20s exit=%s  %s\n' "$defeito" "$?" "$(head -1 /tmp/$defeito.txt)"
done

# e a que sai 1, nao 2
bin/matrix -dir bench/results/c4-invariante-violado; echo "exit=$?"

# tres defeitos ao mesmo tempo saem os tres
bin/matrix -dir bench/results/c4-tres-defeitos
```

Aceite:

- As nove saem `exit=2`, cada uma com a mensagem da sua recusa e nomeando a célula culpada
- `c4-invariante-violado` sai `exit=1`: a distinção entre resultado sobre a engine e ausência de resultado sobrevive à camada nova
- `c4-tres-defeitos` relata os três, não o primeiro
- `accepted-zero` é reprovada mesmo com o `checker.json` inteiro verde — é a recusa que só a matriz é capaz de fazer

### C5 - A matriz inteira

```bash
make down && make up && sleep 20
git status --porcelain   # tem de sair vazio: o pre-voo exige
time bench/run-matrix.sh 2>&1 | tail -40; echo "exit=$?"

MATRIX=$(ls -1dt bench/results/m* | head -1)
jq -r '.publishable, .git.dirty, .poolSize, (.cells | length)' "$MATRIX/matrix.json"
jq -r '.control | "controle: \(.acceptedPerSecond[0]) vs \(.acceptedPerSecond[1]) · divergencia=\(.divergence) · \(.verdict)"' "$MATRIX/matrix.json"
jq -r '[.cells[] | select(.warnings | length > 0) | "\(.order) \(.warnings | join(", "))"] | .[]' "$MATRIX/matrix.json"
jq -r '.cells[] | select(.strategy=="optimistic" and .scenario=="last_second_spike" and .policy=="immediate") | "\(.auctions) leiloes: \(.acceptedPerSecond) aceitos/s, \(.attemptsPerAccept) tentativas/aceito, \(.exhaustedRate) exauridos"' "$MATRIX/matrix.json"
cat "$MATRIX/matrix.md"
```

Aceite:

- As 37 células rodam, todas com `checker.json` em `exit: 0`, entre uma hora e meia e duas horas
- `publishable: true`, `dirty: false`, `poolSize` igual nas 37, 36 linhas na tabela
- O bloco do controle sai com os dois números e o veredito, **mesmo quando OK**
- Cada aviso da matriz aparece na linha da célula que o gerou
- A tabela do `matrix.md` tem as 36 linhas na ordem do plano, e é colável em `benchmark.md` sem edição
- A saída completa do `time` e a tabela vão coladas no PR: é este checkpoint que entrega a etapa

### C6 - O caos continua verde depois do `bench/wait.sh`

```bash
CHAOS=closerd-kill RUN=chaos-wait-check STRATEGY=optimistic ENDS_IN=45s \
  AUCTIONS=10 SCENARIO=ramp POLICY=immediate bench/run-cell.sh; echo "exit=$?"
jq -r '.exit, .chaos.scenario, .chaos.landed' bench/results/chaos-wait-check/checker.json
bash -n chaos/run-all.sh && shellcheck chaos/run-all.sh && echo "run-all intacto"
```

Aceite:

- A célula de caos sai `exit: 0` com o bloco `chaos` presente e `landed` verdadeiro: mover as duas esperas para `bench/wait.sh` não quebrou o laço irmão
- `chaos/run-all.sh` passa em `bash -n` e no `shellcheck` lendo o arquivo novo
- A célula de caos continua **fora** de qualquer matriz: o prefixo `chaos-` a mantém onde a decisão 92 a colocou

## Smoke Manual

Pre-condicoes:

```text
Docker e docker compose v2, jq, shellcheck, go e make instalados
Portas livres: 5432, 6379, 8080, 8081, 9090, 3000
Repositorio limpo (o pre-voo exige), .env criado a partir de .env.example
Maquina ociosa: nada alem do compose do projeto rodando
```

Passos:

```bash
make up && sleep 20

# o plano existe antes de qualquer carga, e nao toca em nada
bench/run-matrix.sh --dry-run | head -4
ls bench/results/ | grep '^m' || echo "nenhum diretorio de matriz criado"

# o pre-voo recusa arvore suja, que e a recusa mais barata do PR
echo "sujeira" >> README.md 2> /dev/null || touch /tmp/sujo && echo x > bench/sujo.txt
bench/run-matrix.sh --dry-run > /dev/null; echo "dry-run com arvore suja: exit=$?"
bench/run-matrix.sh --only '^01-' ; echo "execucao com arvore suja: exit=$?"
rm -f bench/sujo.txt

# uma celula pela mao, com o RUN em forma de matriz: o subdiretorio aterrissa
RUN=msmoke/01-optimistic-a1-ramp-immediate AUCTIONS=1 SCENARIO=smoke \
  POLICY=immediate STRATEGY=optimistic bench/run-cell.sh; echo "exit=$?"
find bench/results/msmoke -type f | sort
jq -r '.run, .durationMs' bench/results/msmoke/01-optimistic-a1-ramp-immediate/client.json
docker ps -a --format '{{.Names}}' | grep k6 || echo "nenhum k6 orfao"

# o agregador nao fala com nada: rode com o compose no chao
make down
bin/matrix -dir bench/results/msmoke; echo "exit=$?"
```

Aceite manual:

- O `--dry-run` imprime o plano e **não** cria diretório nenhum
- Com a árvore suja, o `--dry-run` continua saindo 0 — ele não mede nada — e a execução real sai 2 com a mensagem sobre a árvore. Esta é a recusa que economiza duas horas
- A célula manual escreve os cinco arquivos dentro de `bench/results/msmoke/01-.../`, com o subdiretório intacto: `client.json`, `summary.json`, `env.json`, `checker.txt` e `checker.json`
- O `run` gravado no `client.json` carrega a barra, e `durationMs` é próximo de 15000 para o cenário `smoke`
- Nenhum contêiner do k6 sobrou, apesar da barra no `RUN`: o saneamento do nome funcionou
- Com o compose **no chão**, o agregador ainda roda e recusa por célula faltando. Ele não fala com banco, com Redis nem com Prometheus, e esta é a prova mais curta disso

## Definicao De Pronto

- RF01 a RF15 implementados
- C1 a C6 executados, com a saída real colada no PR — checkpoint sem saída não conta como aceito
- 36 células mais o controle, todas com `checker.json` em `exit: 0`, num único `MATRIX`, num único commit, com `dirty: false`
- `matrix.json` publicável, com as 36 linhas, o bloco do controle e todos os avisos que houver
- A divergência do controle registrada e dentro da banda; se ficar entre 10% e 25%, publicada ao lado do resultado em vez de escondida
- I1 a I8 verdes nas 37, sem um único invariante relaxado — a matriz não tem `chaos.json` e não pode ter
- As nove recusas demonstradas contra artefatos plantados, cada uma com a sua mensagem, e a que sai 1 saindo 1
- `bench/run-cell.sh` com exatamente uma linha alterada, e o nome do contêiner idêntico ao de hoje para todo `RUN` sem barra
- `bench/bid-storm.js` com exatamente um campo novo: cenário, threshold e apostador byte a byte iguais
- `chaos/run-all.sh` lendo `bench/wait.sh`, e um cenário de caos verde para provar
- Nenhuma série nova, nenhum dashboard, nenhum sweep de pool, nenhum `if` de matriz dentro de `internal/`
- `go test ./... -race` limpo, `gofmt -l .` vazio, `shellcheck` sem aviso nos quatro scripts
- Budget respeitado, ou desvio reportado antes de estourar
- Nenhum arquivo dentro de `docs/` alterado pelo PR de implementação
- Com isto o gráfico principal do projeto passa a existir como dado. A spec 02 varre o pool do pessimista sabendo qual é o piso, a spec 03 desenha em cima de um ambiente que já provou ser o mesmo nas 36 células, e a etapa 6 escreve o resultado lendo um arquivo em vez de 37 diretórios
