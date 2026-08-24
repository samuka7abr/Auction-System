# Decisões — Etapa 5

[← índice](../README.md)

Registro das decisões tomadas ao desenhar as specs da etapa 5. A numeração continua a da [etapa 1](etapa-1.md), da [etapa 2](etapa-2.md), da [etapa 3](etapa-3.md) e da [etapa 4](etapa-4.md): as decisões de 1 a 93 continuam valendo, e as que são emendadas aqui dizem em qual ponto.

---

## Decisões da spec 01

Tomadas ao desenhar a [spec 01 da etapa 5](../specs/etapa-5/01-spec-matriz.md), a matriz de 36 células. Duas delas **emendam** [benchmark.md](../projeto/benchmark.md): a **94** substitui o laço de exemplo da seção *Isolamento entre células*, e a **104** precisa a frase *"mesmo volume total de requisições"*. Onde houver divergência, vale o que está aqui.

---

### 94. A estratégia é o eixo mais interno, e o `auctiond` é recriado a cada célula

A ordem de execução das 36 células é **contenção → cenário → política → estratégia**, com a estratégia mudando a cada linha. As três engines comparadas num ponto do gráfico rodam adjacentes no tempo.

**Por quê:** a alternativa é agrupar por estratégia, recriar o `auctiond` três vezes em vez de 37 e economizar uns nove minutos. Ela **confunde estratégia com tempo**. Se a máquina esquentar, se o cache de página encher, se o Postgres acumular qualquer coisa ao longo de uma hora e meia, a deriva entra no gráfico com nome de estratégia — e o gráfico é a diferença entre estratégias. Seria pagar com a variável sob teste para economizar nove minutos.

Com a estratégia por dentro, a comparação que o projeto publica acontece dentro de uma janela de minutos, sob condições de máquina praticamente idênticas. A deriva não some, mas passa a afetar as três curvas quase igualmente, que é o máximo que um harness deste tamanho consegue prometer honestamente.

**O que o recreate por célula compra além disso:** o **processo** também passa a partir do mesmo estado. Heap, pool aberto, janela de supressão em memória do fechamento (decisão 70) e os inboxes do shard nascem do zero em toda célula, em vez de herdados da célula anterior. A decisão 13 mandou resetar o banco entre células porque a última rodaria sobre uma tabela inchada; a mesma frase vale para um processo que já rodou onze células. Isto é aquela decisão aplicada ao lado que faltava.

**Emenda a `benchmark.md`:** o laço de exemplo da seção *Isolamento entre células* mostra `psql`, `go run ./cmd/seed` e `k6 run` soltos, e não mostra troca de engine nenhuma. O que roda de verdade é `bench/run-cell.sh`, que já faz reset, seed, `VACUUM`, `FLUSHALL`, warmup, segundo reset, carga e checker — e o `bench/run-matrix.sh` acrescenta por fora o recreate do `auctiond` e a prova de qual engine está no ar.

**Custo aceito:** ~15 s por recreate, ~9 minutos numa execução de ~1h30.

---

### 95. A célula-controle é a primeira repetida por último, e a divergência tem banda

A célula 37 é a célula 01 outra vez. O agregador compara `aceitos/s` das duas e aplica três faixas: abaixo de 10% é OK, entre 10% e 25% é aviso publicado, acima de 25% reprova a matriz inteira.

**Por que uma banda, e não uma comparação:** `benchmark.md` diz *"se os dois números divergirem, houve efeito de ordem e a matriz não vale"*. Dito assim, qualquer execução real reprova: dois benchmarks idênticos na mesma máquina divergem alguns por cento por ruído, sempre. O controle **não separa deriva de ruído** — ele mede os dois somados —, e uma banda é a única forma honesta de usar um instrumento que mede duas coisas juntas.

**Por que 25%:** é o ponto em que a distância entre duas curvas do gráfico principal poderia ser inteiramente explicada pela ordem de execução. Acima disso o entregável do projeto deixa de significar alguma coisa, e publicar seria pior do que não ter rodado.

**Por que 10%, e por que aviso em vez de silêncio:** abaixo de 10% este projeto não consegue distinguir deriva de ruído, e fingir que consegue seria inventar precisão. Entre 10% e 25% a matriz vale, e o leitor precisa saber com que margem — então o número vai publicado ao lado do gráfico, em vez de virar rodapé de arquivo.

**O bloco do controle é publicado mesmo quando OK.** Um controle que só aparece quando falha é um controle que o leitor não sabe se rodou.

---

### 96. A matriz lê três artefatos, e nunca o Prometheus nem o `summary.json`

`env.json`, `client.json` e `checker.json`. O agregador não abre `summary.json`, não consulta o Prometheus e não fala com o banco.

**Por quê:** é a decisão 4 outra vez, mais o motivo que abre `cmd/checker/client.go`. Perguntar ao Prometheus seria publicar o número que o próprio servidor diz sobre si; ler o `summary.json` seria depender de um formato interno ao k6, que muda entre versões e — pior — muda **em silêncio**, com um campo virando `null` em vez de sumir. O `client.json` existe justamente para ser o meio-termo estável entre o k6 e o resto do projeto, e a matriz herda o contrato inteiro, ponteiros incluídos: ausente nunca é lido como zero.

**Consequência aceita:** a matriz publica a latência **do lado do cliente**, que é a única que ela tem. A do servidor é `bid_confirm_duration_seconds{strategy}`, e a distância entre as duas curvas é fila antes do handler — assunto dos dashboards da spec 03, e da leitura da etapa 6.

**O Prometheus continua de pé em toda célula.** Ele faz parte do ambiente fixado e do custo que as três engines pagam igualmente. Ele só não é fonte de número publicado.

---

### 97. `durationMs` entra no `client.json`, e o `cmd/checker` não passa a exigi-lo

Um campo novo no `client.json`, lido de `data.state.testRunDurationMs`, e nenhuma outra mudança em `bench/bid-storm.js`.

**Por que um campo novo:** taxa por segundo precisa de janela, e nenhuma das janelas existentes serve. `startedAt`/`finishedAt` do `env.json` cobrem reset, seed, `VACUUM`, warmup, segundo reset e o checker — numa célula de `last_second_spike` isso é mais de um minuto de relógio em cima de 15 segundos de carga, e `aceitos/s` sairia com um quarto do valor real. A duração nominal do cenário também não serve para o `ramp`, que sobe de 0 a 500 VUs: a janela nominal existe, mas a carga dentro dela não é constante e o k6 é quem sabe quando o teste realmente começou e terminou.

**Por que ele estoura em vez de virar zero:** mesma regra de `count` e `trend`. Um formato de summary que mudou sob upgrade do k6 tem de quebrar alto e na hora, e não três passos adiante como uma taxa verde calculada sobre nada.

**Por que o checker não passa a exigir:** o `cmd/checker` exige o que lê, e ele não lê duração — nenhum dos oito invariantes depende de tempo de execução. Quem exige é o agregador, que recusa a matriz inteira se faltar (recusa R7). É a separação de camadas do projeto dita mais uma vez: **o checker julga corretude, a matriz julga publicabilidade**. Uma célula pode ser perfeitamente correta e mesmo assim não render linha de tabela.

---

### 98. Tentativas por aceito é `attempts ÷ accepted`, e não o trend que já existe

A coluna da matriz é a razão global entre os dois contadores do `client.json`. O trend `clientAttemptsPerAccept`, que já existe, **não** é a coluna.

**Por quê:** o trend é amostrado no aceite. Quem tentou muito e desistiu cai em `bids_exhausted` e nunca entra na amostra. O efeito é o oposto do que a coluna promete ao leitor:

> Quando a disputa sobe, mais apostadores desistem sem aceitar; a amostra do trend passa a conter só os que conseguiram, que são justamente os que precisaram de menos tentativas — e **o trend cai enquanto a contenção sobe**.

Publicar essa curva na tabela do gráfico principal seria publicar a inversão do fenômeno com o nome do fenômeno. A razão global não tem o viés: `attempts` conta toda tentativa feita, `accepted` conta todo aceite, e a divisão é a amplificação que o cliente realmente pagou.

**O trend fica onde está.** Ele responde outra pergunta — a distribuição da amplificação **entre os que conseguiram** — e é uma pergunta legítima da etapa 6. Ter os dois com o mesmo nome, em lugares diferentes, seria a forma mais eficiente de alguém plotar o errado; por isso um se chama `attemptsPerAccept` na matriz e o outro `clientAttemptsPerAccept` no `client.json`, como a **decisão 38** já tinha feito entre o cliente e o servidor.

**E `bids_exhausted` continua coluna própria**, porque é ela que carrega o que o trend esconde. `benchmark.md` já dizia: sob alta contenção, a taxa de exaustão **é** o colapso do otimista tornado visível.

---

### 99. O `RUN` da matriz carrega subdiretório, e o nome do contêiner é saneado

Cada célula roda com `RUN=$MATRIX/<nome>`, e o `bench/run-cell.sh` ganha uma linha que sanea o nome do contêiner do k6.

**Por que aninhar:** 37 diretórios soltos em `bench/results/`, ao lado de `c1`, `c3`, `chaos-*` e de todas as matrizes anteriores, é um diretório que ninguém consegue ler. Aninhados, o `matrix.json` mora ao lado das células que ele agrega, uma matriz nova não se mistura com a anterior, e a retomada tem um escopo óbvio para o seu `rm -rf`.

**Por que uma linha em vez de um prefixo achatado:** a alternativa é `m2026...-01-optimistic-a1-ramp-immediate` como nome plano, sem tocar em nada. Ela custa zero e deixa o `matrix.json` sem casa e a agregação sem escopo. `RESULTS`, `mkdir -p`, o `RESULTS_DIR` do k6 e o `-run` do checker já lidam com barra hoje; o único ponto que não lida é `docker compose run --name`, porque nome de contêiner não aceita barra. Uma linha resolve, e para todo `RUN` sem caractere especial — os de hoje, os do caos, os que uma pessoa digita — o saneamento é a identidade e o nome do contêiner sai idêntico ao de antes.

**O que isto não é:** não é uma mudança de comportamento do `run-cell.sh`. A spec 02 da etapa 4 exigiu que a célula sem `CHAOS` se comportasse exatamente como antes daquele PR, e essa exigência continua valendo aqui: a única diferença observável é para um `RUN` que hoje **não funcionaria**.

---

### 100. Retomada só dentro do mesmo commit, e a célula refeita perde os artefatos antigos

`RESUME=1` pula uma célula cujo `checker.json` existe e traz `exit: 0`. Qualquer outro estado refaz a célula, apagando o diretório antes. E o pré-voo recusa retomar se o commit gravado nas células já prontas não for o `HEAD`.

**Por que existe retomada:** uma matriz custa entre uma hora e meia e duas horas. Perder tudo porque a célula 30 encontrou um `docker` mal-humorado é caro o bastante para justificar quinze linhas de script.

**Por que só `exit: 0` conta como pronta:** uma célula sem `checker.json`, com código diferente de zero ou com JSON ilegível não é uma célula pronta, é um destroço. Aproveitá-la seria publicar um número que ninguém verificou.

**Por que o diretório é apagado antes de refazer:** um `client.json` sobrevivente de um k6 que morreu no meio seria verificado, na tentativa seguinte, contra um banco escrito por outra execução. O checker acharia divergência entre banco e cliente e reportaria I5 — um falso positivo do harness dentro do invariante que existe para pegar write perdido. Apagar é mais barato do que explicar isso depois.

**Por que a retomada não atravessa commit:** duas células de códigos diferentes não são a mesma matriz, e o agregador já recusaria isso (recusa R4). O pré-voo só move a recusa para onde ela custa menos: o segundo zero, em vez do minuto noventa.

**A guarda do `rm -rf`:** caminho sob `bench/results/$MATRIX/`, nome pertencente ao plano, `$MATRIX` não vazio. É a única linha do PR capaz de destruir trabalho alheio, e ela não é escrita sem as três.

---

### 101. A matriz para na primeira célula não-verde, e o código sai sem tradução

O laço aborta na primeira célula com código diferente de zero, imprime o nome da célula, o código e o comando de retomada — e sai com o mesmo código, sem colapsar nada.

**Por quê:** é a decisão 93 sendo cobrada pela primeira vez. O `cmd/checker` distingue invariante violado (1) de célula não verificável (2) desde a etapa 1, e o `run-cell.sh` deixa o 99 do k6 passar quando o threshold estoura. Cada um desses três códigos manda fazer uma coisa diferente:

| Código | Significa | O que fazer |
| --- | --- | --- |
| 1 | Invariante violado | Achado sobre a engine. Ler, e virar spec própria |
| 2 | Célula não verificável | Ausência de resultado. Consertar a causa e retomar |
| 99 | `http_req_failed` acima de 1% | Achado sobre o limite do sistema (decisão 102) |

Passar o laço por dentro do `make` transformaria os três em 2, que é exatamente o que a decisão 93 previu. Por isso `make matrix` é uma linha que chama `bench/run-matrix.sh`, e automação chama o script.

**Por que parar em vez de seguir:** uma matriz que continua depois de uma célula vermelha gasta mais uma hora produzindo linhas que ninguém vai poder publicar — o agregador vai recusar a matriz inteira de qualquer jeito. E, se o vermelho for invariante violado, o que está na tela é o achado mais valioso que este projeto sabe produzir: ele merece ser lido antes de a máquina fazer mais qualquer coisa.

---

### 102. O `99` continua abortando fora do caos, e afrouxar seria escolher terminar em vez de medir

`http_req_failed: rate<0.01` estourado aborta a célula, e a matriz para. A exceção da decisão 83 vale só sob `CHAOS`.

**Por quê:** a leitura da etapa 1 continua correta. 409 e 422 não entram no `http_req_failed`, 410 e 425 também não, e um apostador que desiste no `BID_DEADLINE` não gera erro nenhum. O que resta acima de 1% é infraestrutura caindo — 5xx, 404, 400, transporte — e uma linha de resultado publicada em cima disso é uma mentira com unidade. O I6 concorda pelo mesmo número (`maxErrorRate = 0.01`), então a célula seria reprovada duas vezes.

**A tentação, dita em voz alta:** se a engine pessimista sob 1000 VUs devolver 503 acima de 1% porque o pool saturou, a matriz para e não termina. A saída fácil é afrouxar o threshold para a matriz terminar. Ela está errada, e por um motivo específico: **isso é o limite da engine aparecendo**, e é resultado — mas resultado que precisa ser medido de propósito, com o erro como variável, e não colhido de acidente numa célula cujo objetivo era outro. Uma matriz que termina porque baixou a régua não mediu nada que valha publicar.

Se acontecer, é achado, e o achado gera spec — a mesma regra que a decisão 90 já aplicou ao `/readyz` acoplado a um pool saturado.

---

### 103. São 36 células e não 24: a política de retentativa vale para as três engines

A matriz roda `immediate` e `jitter` também para `pessimistic` e `shard`.

**Por que a dúvida é razoável:** `benchmark.md` diz que pessimista e shard *"ignoram a política: elas não produzem 409"*. Lido rápido, isso sugere 12 células duplicadas e uma matriz de 24.

**Por que está errado:** o backoff do apostador é aplicado **também no 422**. Ser superado é o desfecho comum das três engines, e nas três o cliente re-mira e espera antes de tentar de novo. Trocar `immediate` por `jitter` muda a carga oferecida nas três — o intervalo entre tentativas, a distribuição de chegada, o número de tentativas que cabem dentro do `BID_DEADLINE`.

O que continua verdade é que a política **importa muito mais** na otimista, porque lá ela governa 409 e 422 ao mesmo tempo. A matriz mede o tamanho dessa diferença em vez de assumi-lo — e é essa medição que responde a crítica que `benchmark.md` chama de *"a mais forte que se pode fazer ao projeto inteiro"*: se o jitter salvar o otimista, isso falsifica a hipótese, e é resultado.

---

### 104. Entre contenções, o que é constante é a carga oferecida, e não a entregue

Os três níveis de contenção rodam o mesmo cenário: mesmos VUs, mesma duração, mesmo apostador. O número de requisições **entregues** não é idêntico entre eles, e não pode ser.

**Por quê:** sob contenção alta a latência sobe e cada VU completa menos iterações. Forçar o mesmo número de requisições entregues significaria fechar na mão a variável dependente, e o gráfico passaria a medir a paciência do gerador em vez do mecanismo de concorrência.

`aceitos/s` é comparável porque a carga oferecida é idêntica. E a diferença entre oferecido e entregue não some: ela está em `bids_exhausted` e na razão de tentativas por aceito, que são colunas da tabela justamente por isso.

**Emenda a `benchmark.md`:** onde se lê *"o mesmo volume total de requisições distribuído sobre 1, 10 ou 1000 leilões"*, entenda-se o mesmo **volume oferecido** — cenário, VUs e duração idênticos. Está registrado aqui porque a alternativa é alguém ler a tabela em 2027, notar que as células de 1000 leilões entregaram mais requisições, e concluir que houve erro de método.

---

### 105. O pool fica fixo em 25 nas 36 células, e o agregador reprova se variar

`DB_POOL_SIZE` é constante na matriz, e `poolSize` divergente entre células é recusa (R6).

**Por quê:** é o terceiro invariante de método de [README.md](../README.md) — *"pool, CPU e memória fixos e iguais nas três"* —, e sem ele a matriz mede uma configuração e chama de resultado. A verificação existe porque a spec 02 desta etapa vai varrer exatamente essa variável: o dia em que alguém rodar o sweep e a matriz na mesma sessão, uma variável de ambiente esquecida no shell é a forma mais provável de as duas se misturarem, e o `env.json` de cada célula é o único lugar onde isso ficaria registrado.

**Por que 25:** é o que está no `.env.example` desde a etapa 1, é o que as etapas 2, 3 e 4 mediram, e trocar agora seria trocar a base de comparação junto com o experimento. Qual pool é o certo é a pergunta da spec 02, e ela responde variando o pool com a matriz parada.

---

### 106. Aviso é publicado, não escondido

Três condições marcam a linha da célula sem reprovar a matriz: gerador saturado, exaustão acima de 20%, e o controle divergindo entre 10% e 25%. Os avisos do próprio `checker.json` são carregados junto.

**Por quê:** todos os três são casos em que o número existe, é legítimo e **pode ser do instrumento e não do sistema**. Reprovar seria descartar dado bom; omitir seria publicar dado sem a ressalva que ele exige. Marcar é a única das três opções que deixa a decisão com quem lê.

| Aviso | O que o leitor precisa saber |
| --- | --- |
| Gerador saturado | Acima de 90% do próprio limite, o `aceitos/s` pode ser do k6 e não do `auctiond` (o I6 já avisa; a matriz carrega para a tabela) |
| Exaustão > 20% | O limiar de `benchmark.md`: acima dele `MAX_RETRIES` está mascarando o efeito |
| Controle 10–25% | A margem de deriva com que este gráfico foi medido |

**O caso da exaustão merece uma frase a mais:** `benchmark.md` diz que acima de ~20% o `MAX_RETRIES` precisa subir. Subir `MAX_RETRIES` **obriga a rodar as 36 células de novo** — uma matriz com dois apostadores diferentes não é uma matriz —, e essa é uma decisão da etapa 6 tomada com a tabela na mão, não um ajuste feito no meio do laço.

---

### 107. Nove recusas, todas relatadas, e a que sai 1 sai 1

O agregador tem nove condições que impedem a publicação. Ele relata **todas** as que encontrar, não a primeira. E `failures > 0` em alguma célula sai com 1, não com 2.

**Por que nove, e por que cada uma:** cada recusa existe porque a linha correspondente seria plausível e falsa — célula faltando, diretório intruso, célula não verificada, caos misturado, commit divergente ou árvore suja, estratégia trocada, pool diferente, duração ausente, aceites zerados, controle estourado.

**Cinco delas o `cmd/checker` é incapaz de fazer**, e não por descuido: ele verifica uma célula por vez, contra um banco que acabou de ser resetado. Ele não sabe em que diretório a célula vai cair, qual commit produziu a vizinha, nem o que as outras 36 fizeram. O caso extremo é a estratégia trocada: uma célula perfeitamente verde, cujo `env.json` diz `pessimistic` e cujo diretório diz `optimistic`, vira uma linha plausível e falsa na tabela — *"o erro mais caro possível numa matriz de 36"*, na frase que o próprio `run-cell.sh` usa para justificar o seu pré-voo. É por isso que a camada existe.

**Por que todas de uma vez:** uma matriz com três problemas relatando um por execução custa três execuções para descobrir o terceiro — e cada execução, se o conserto exigir rodar células, custa horas.

**Por que 1 continua sendo 1:** um invariante violado é um resultado sobre a engine; uma matriz incoerente é a ausência de resultado. É a mesma distinção que o `cmd/checker` mantém desde a etapa 1, e ela precisa sobreviver à camada nova ou a camada nova a apaga — exatamente o que a decisão 93 impediu que o `make` fizesse.

---

### 108. `bench/results/` é local, e quem publica número é a etapa 6

O diretório está no `.gitignore` desde a etapa 1, e continua. O `matrix.json` e o `matrix.md` não entram no repositório por este PR.

**Por quê:** artefato de medição não é código, e versionar 37 diretórios de JSON por execução transformaria o histórico do repositório num depósito de dados. O que é durável, e vai para `docs/`, é o **resultado lido**: a tabela preenchida em `benchmark.md`, o gráfico de cruzamento e o texto que os explica — e isso é a etapa 6, com revisão de texto e não de shell.

**O que este PR entrega, então:** o instrumento e a prova de que ele rodou — a saída real colada no PR, checkpoint por checkpoint. O `matrix.md` existe justamente para ser o formato colável: a etapa 6 lê um arquivo em vez de 37 diretórios, e a transcrição de 36 linhas na mão deixa de ser uma fonte de erro.

---

### 109. As duas esperas do compose viram `bench/wait.sh`, lido pelos dois laços

`wait_ready` e `wait_strategy` saem de `chaos/run-all.sh` para um arquivo próprio, `source`ado pelo laço do caos e pelo da matriz. `check_cure` não se move.

**Por que extrair, num projeto que prefere repetição a abstração ruim:** porque o que essas duas funções codificam não é forma, é um fato. `wait_strategy` pergunta ao `/metrics` por `bid_confirm_duration_seconds_count{strategy="..."}` porque essa série é a **prova de qual engine o processo está rodando** — não o que o compose pediu, o que o processo faz. É a mesma verificação que o pré-voo do `run-cell.sh` faz, pelo mesmo motivo escrito lá: *"a célula rodada contra a engine errada produz uma linha plausível, falsa, e não deixa rastro"*.

Duas cópias desse fato significam que, no dia em que o nome da série mudar, uma delas fica para trás — e a que ficar para trás trava 90 segundos e derruba uma matriz de duas horas pela metade. O injetor de caos ganhou o direito de repetir quatro falhas que não se parecem, e o budget da spec 02 da etapa 4 escreveu por quê — não dá para fatorá-las sem inventar uma abstração pior do que a repetição. Duas esperas idênticas, sobre a mesma série, com o mesmo deadline, não são o mesmo caso.

**Por que `check_cure` fica:** ela é do caos. A matriz não pausa Redis, não segura linha e não mata contêiner — não tem o que curar. Mover uma função para um arquivo compartilhado porque ela é vizinha da que precisava mudar é como abstrações ruins começam.

**O preço, pago no checkpoint:** este PR mexe num arquivo do PR anterior, então precisa provar que não o quebrou. O C6 roda um cenário de caos de ponta a ponta e confere o `chaos.json` — dez minutos para não descobrir isso na próxima etapa.
