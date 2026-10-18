export namespace config {
	
	export class InternetProbe {
	    url: string;
	    expect_status: number;
	    expect_body: string;
	
	    static createFrom(source: any = {}) {
	        return new InternetProbe(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.expect_status = source["expect_status"];
	        this.expect_body = source["expect_body"];
	    }
	}
	export class Config {
	    username: string;
	    password: string;
	    carrier: string;
	    internet_probe: InternetProbe;
	    poll_interval_seconds: number;
	    autostart: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Config(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.username = source["username"];
	        this.password = source["password"];
	        this.carrier = source["carrier"];
	        this.internet_probe = this.convertValues(source["internet_probe"], InternetProbe);
	        this.poll_interval_seconds = source["poll_interval_seconds"];
	        this.autostart = source["autostart"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace main {
	
	export class ProbePreset {
	    name: string;
	    url: string;
	    status: number;
	    body: string;
	    note: string;
	
	    static createFrom(source: any = {}) {
	        return new ProbePreset(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.url = source["url"];
	        this.status = source["status"];
	        this.body = source["body"];
	        this.note = source["note"];
	    }
	}
	export class SaveResult {
	    ok: boolean;
	    errors: string[];
	    warnings: string[];
	    path: string;
	
	    static createFrom(source: any = {}) {
	        return new SaveResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.errors = source["errors"];
	        this.warnings = source["warnings"];
	        this.path = source["path"];
	    }
	}

}

export namespace monitor {
	
	export class Attempt {
	    // Go type: time
	    at: any;
	    ok: boolean;
	    verdict: string;
	    summary: string;
	    elapsed: number;
	
	    static createFrom(source: any = {}) {
	        return new Attempt(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.at = this.convertValues(source["at"], null);
	        this.ok = source["ok"];
	        this.verdict = source["verdict"];
	        this.summary = source["summary"];
	        this.elapsed = source["elapsed"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Snapshot {
	    state: string;
	    detail: string;
	    probe_kind: string;
	    probe_detail: string;
	    // Go type: time
	    last_check_at: any;
	    // Go type: time
	    next_retry_at: any;
	    // Go type: time
	    online_since: any;
	    consecutive_failures: number;
	    config_error: string;
	    last_attempt?: Attempt;
	    history: Attempt[];
	    portal_address: string;
	
	    static createFrom(source: any = {}) {
	        return new Snapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.state = source["state"];
	        this.detail = source["detail"];
	        this.probe_kind = source["probe_kind"];
	        this.probe_detail = source["probe_detail"];
	        this.last_check_at = this.convertValues(source["last_check_at"], null);
	        this.next_retry_at = this.convertValues(source["next_retry_at"], null);
	        this.online_since = this.convertValues(source["online_since"], null);
	        this.consecutive_failures = source["consecutive_failures"];
	        this.config_error = source["config_error"];
	        this.last_attempt = this.convertValues(source["last_attempt"], Attempt);
	        this.history = this.convertValues(source["history"], Attempt);
	        this.portal_address = source["portal_address"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class TestResult {
	    ok: boolean;
	    message: string;
	    probe_kind: string;
	    probe_detail: string;
	    http_status: number;
	    response_head: string;
	    portal_address: string;
	
	    static createFrom(source: any = {}) {
	        return new TestResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.message = source["message"];
	        this.probe_kind = source["probe_kind"];
	        this.probe_detail = source["probe_detail"];
	        this.http_status = source["http_status"];
	        this.response_head = source["response_head"];
	        this.portal_address = source["portal_address"];
	    }
	}

}

