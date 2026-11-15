# CategorizeTasks

Webhook em Go (Vercel) que imita o "auto-fill" do Notion: quando o título ou a
descrição de uma página de uma database muda, pede ao Claude Haiku 5.5 uma
categoria e preenche um campo `select`.

## Como funciona

1. O Notion manda `page.created` / `page.properties_updated` para `/api/webhook`.
2. O handler valida a assinatura (`X-Notion-Signature`, HMAC-SHA256 com o
   `verification_token`).
3. Busca a página. Em `properties_updated`, só segue se o título ou a descrição
   estiverem em `updated_properties`. Isso também evita loop, porque a nossa
   própria escrita na categoria gera um evento que só lista a categoria.
4. Lê as opções do `select` direto do schema da database (data source). Para
   mudar as categorias, basta editar as opções no Notion. A descrição de cada
   opção, se existir, também vai para o modelo.
5. Chama o `claude-haiku-5-5` com structured output (um `enum` com as opções),
   então a resposta é sempre uma categoria válida.
6. Atualiza o `select`, a não ser que a categoria já seja a mesma.

Falhas temporárias (429/5xx, timeout) retornam 503, e o Notion reenvia o
evento. Erros de configuração são logados e respondidos com 200, para o Notion
não ficar reenviando.

## Setup

1. **Integração Notion:** crie uma em <https://www.notion.so/profile/integrations>,
   copie o token e compartilhe a database com ela (`...` > Connections).
2. **Database:** precisa de um título, de uma propriedade de texto
   `Description` e de um `select` `Category` com as opções. Os nomes podem
   ser trocados pelas variáveis de ambiente.
3. **Deploy:**
   ```bash
   vercel link
   vercel env add NOTION_TOKEN
   vercel env add ANTHROPIC_API_KEY
   vercel env add NOTION_DATABASE_ID        # opcional
   vercel --prod
   ```
4. **Webhook:** na aba *Webhooks* da integração, crie uma assinatura para
   `https://<seu-app>.vercel.app/api/webhook` com os eventos `page.created` e
   `page.properties_updated`.
5. **Verificação:** o Notion manda um `verification_token`, que aparece nos
   logs (`vercel logs` ou no dashboard) como `notion verification_token: ...`.
   Cole o token no Notion para ativar a assinatura e salve como secret:
   ```bash
   vercel env add NOTION_WEBHOOK_SECRET
   vercel --prod
   ```
   Até essa variável existir, os eventos são aceitos sem checar a assinatura.

## Pausar

Com `WEBHOOK_PAUSED=true`, o webhook responde 200 e descarta os eventos, sem
chamar o Notion nem o modelo. Os eventos recebidos nesse período se perdem
(não ficam numa fila). Na Vercel, a variável só vale depois de um redeploy:

```bash
vercel env add WEBHOOK_PAUSED production   # valor: true
vercel --prod
# para retomar:
vercel env rm WEBHOOK_PAUSED production && vercel --prod
```

## Desenvolvimento local

```bash
cp .env.example .env   # preencha
set -a; . ./.env; set +a
go run ./cmd/local     # http://localhost:8080/api/webhook
ngrok http 8080        # URL pública para o Notion
go test ./...
```
