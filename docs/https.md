# HTTPS without Caddy

The README starts Caddy next to GitCove for HTTPS. This page covers two other ways: nginx in front of GitCove, or GitCove serving HTTPS itself.

Either way, first create the DNS record (step 2 of the README) and make sure ports 80 and 443 are reachable from the internet. If you already started the `caddy` container from the README, remove it so it frees those ports: `sudo docker rm -f caddy`. The commands below are for Debian or Ubuntu. Then set your domain name in the server's terminal:

```sh
DOMAIN=gitcove.example.com
```

## nginx with certbot

nginx forwards requests to GitCove. certbot gets a free certificate from Let's Encrypt and renews it automatically.

1. Install both:

   ```sh
   sudo apt install -y nginx certbot python3-certbot-nginx
   ```

2. Create the nginx site for GitCove and turn it on:

   ```sh
   sudo tee /etc/nginx/sites-available/gitcove >/dev/null <<EOF
   server {
       listen 80;
       server_name $DOMAIN;

       # Git pushes can be large: no upload limit, and an hour before slow ones time out
       client_max_body_size 0;
       proxy_read_timeout 1h;
       proxy_send_timeout 1h;

       location / {
           proxy_pass http://127.0.0.1:8418;
           proxy_set_header Host \$host;
           proxy_set_header X-Forwarded-Proto \$scheme;
           proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
       }
   }
   EOF
   sudo ln -s /etc/nginx/sites-available/gitcove /etc/nginx/sites-enabled/gitcove &&
   sudo nginx -t && sudo systemctl reload nginx
   ```

   `nginx -t` should say the configuration is ok.

3. Get the certificate:

   ```sh
   sudo certbot --nginx -d "$DOMAIN"
   ```

   certbot asks for an email address and asks you to accept Let's Encrypt's terms. It then adds HTTPS to the nginx site and redirects plain HTTP to HTTPS. It also sets up automatic renewal. `sudo certbot renew --dry-run` checks that renewal works.

4. Check: `curl https://$DOMAIN/status` should print the same output as `curl localhost:8418/status`.

Both settings above matter. Without `X-Forwarded-Proto`, GitCove thinks the connection is plain HTTP. Without `client_max_body_size 0`, nginx refuses uploads over 1 MB, so large pushes fail.

## GitCove serves HTTPS itself

No proxy: GitCove listens on port 443 with a certificate you give it (`COVE_TLS_CERT` and `COVE_TLS_KEY`). It reads the certificate only at startup, so it has to restart after each renewal. The steps below set that up.

1. Install certbot and get a certificate. Port 80 must be free while it runs:

   ```sh
   sudo apt install -y certbot &&
   sudo certbot certonly --standalone -d "$DOMAIN"
   ```

   certbot asks for an email address and asks you to accept Let's Encrypt's terms.

2. Add a script that copies the certificate to where GitCove can read it, then restarts GitCove. certbot runs it after every renewal:

   ```sh
   sudo tee /etc/letsencrypt/renewal-hooks/deploy/gitcove >/dev/null <<EOF
   #!/bin/sh
   install -d -o 10001 -m 700 /etc/gitcove-tls
   install -o 10001 -m 600 /etc/letsencrypt/live/$DOMAIN/fullchain.pem /etc/gitcove-tls/cert.pem
   install -o 10001 -m 600 /etc/letsencrypt/live/$DOMAIN/privkey.pem /etc/gitcove-tls/key.pem
   docker restart gitcove
   EOF
   sudo chmod 755 /etc/letsencrypt/renewal-hooks/deploy/gitcove &&
   sudo /etc/letsencrypt/renewal-hooks/deploy/gitcove
   ```

3. Recreate the GitCove container so it listens on port 443 with the certificate. Your data stays on the `gitcove-data` volume. Set `ORG`, `APP_ID` and `KEY` again, as in step 3 of the README, then paste:

   ```sh
   sudo docker rm -f gitcove &&
   sudo docker run -d --name gitcove --restart unless-stopped -p 443:8418 \
     -v gitcove-data:/data -v /etc/gitcove-app.pem:/run/app.pem:ro \
     -v /etc/gitcove-tls:/run/tls:ro \
     -e COVE_ORG="$ORG" -e COVE_APP_ID="$APP_ID" -e COVE_APP_KEY_FILE=/run/app.pem \
     -e COVE_TLS_CERT=/run/tls/cert.pem -e COVE_TLS_KEY=/run/tls/key.pem \
     ghcr.io/tabaak/gitcove:latest
   ```

   After this, GitCove is no longer reachable at `localhost:8418` on the server, only at `https://$DOMAIN`.

4. Check: `curl https://$DOMAIN/status` should print GitCove's status. `sudo certbot renew --dry-run` checks that renewal works.
