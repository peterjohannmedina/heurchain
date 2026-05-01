# /opt/memory-broker/reseed.py
import redis

def trigger_reseed():
    """Publish a reseed trigger to Redis pub/sub channel."""
    try:
        r = redis.StrictRedis(host="localhost", port=6379, db=0, decode_responses=True)
        r.publish("__rmh_reseed__", "reseed")
        print("Reseed trigger published to __rmh_reseed__")
    except Exception as e:
        print(f"Reseed trigger failed (non-fatal): {e}")

if __name__ == "__main__":
    trigger_reseed()
